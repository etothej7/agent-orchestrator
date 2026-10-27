package store_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	storysvc "github.com/aoagents/agent-orchestrator/backend/internal/service/story"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

// Done is the manager's decision. These tests pin who may record it and that the independent
// review evidence for the current revision is still required.
type doneGuardFixture struct {
	t       *testing.T
	ctx     context.Context
	db      *sqlite.Store
	svc     *storysvc.Service
	seq     int
	worker  domain.SessionID
	review  domain.SessionID
	manager domain.SessionID
	// otherManager is the manager of ANOTHER repository's story; coordinator runs the overall project.
	otherManager domain.SessionID
	coordinator  domain.SessionID
}

func newDoneGuardFixture(t *testing.T) *doneGuardFixture {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, id := range []string{"repo", "repo2", "coordinator"} {
		seedProject(t, db, id)
	}
	if err := db.SetCoordination(ctx, domain.CoordinationConfig{CoordinatorProjectID: "coordinator", ManagedProjectIDs: []domain.ProjectID{"repo", "repo2"}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	session := func(project string, kind domain.SessionKind) domain.SessionID {
		rec := sampleRecord(project)
		rec.Kind = kind
		created, err := db.CreateSession(ctx, rec)
		if err != nil {
			t.Fatal(err)
		}
		return created.ID
	}
	f := &doneGuardFixture{t: t, ctx: ctx, db: db, svc: &storysvc.Service{Store: db, Messenger: &storyMessenger{}},
		worker: session("repo", domain.KindWorker), review: session("repo", domain.KindWorker), manager: session("repo", domain.KindOrchestrator),
		otherManager: session("repo2", domain.KindOrchestrator), coordinator: session("coordinator", domain.KindOrchestrator)}
	if err := f.svc.CreateEpic(ctx, domain.Epic{ID: "e", Title: "TEST"}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *doneGuardFixture) apply(story, kind string, sender, recipient domain.SessionID, role, ref, outcome string) error {
	f.t.Helper()
	row, err := f.db.GetStory(f.ctx, story)
	if err != nil {
		f.t.Fatal(err)
	}
	f.seq++
	_, err = f.svc.Apply(f.ctx, storysvc.Command{ID: fmt.Sprintf("cmd-%d", f.seq), StoryID: story, Revision: row.Revision, Kind: kind, SenderSessionID: sender, RecipientSessionID: recipient, Role: role, Text: "Public explicit result", Reference: ref, Outcome: outcome})
	return err
}

func (f *doneGuardFixture) must(story, kind string, sender, recipient domain.SessionID, role, ref, outcome string) {
	f.t.Helper()
	if err := f.apply(story, kind, sender, recipient, role, ref, outcome); err != nil {
		f.t.Fatalf("%s %s: %v", story, kind, err)
	}
}

// readyStory creates a story with a worker and a reviewer (plus a manager when withManager) and
// brings it to ready_for_approval with matching implementation, passing checks and independent review.
func (f *doneGuardFixture) readyStory(id string, withManager bool) {
	f.t.Helper()
	if err := f.svc.Create(f.ctx, domain.Story{ID: id, EpicID: "e", Title: "TEST " + id, ProjectIDs: []domain.ProjectID{"repo", "repo2"}}); err != nil {
		f.t.Fatal(err)
	}
	f.must(id, "assignment", "", f.worker, "worker", "", "")
	f.must(id, "assignment", "", f.review, "reviewer", "", "")
	if withManager {
		f.must(id, "assignment", "", f.manager, "manager", "", "")
	}
	f.must(id, "implementation", f.worker, "", "", "sha", "")
	f.must(id, "testing", f.worker, "", "", "sha", "passed")
	f.must(id, "review_passed", f.review, "", "", "sha", "")
	f.must(id, "ready_for_approval", "", "", "", "", "")
}

func (f *doneGuardFixture) status(id string) string {
	f.t.Helper()
	row, err := f.db.GetStory(f.ctx, id)
	if err != nil {
		f.t.Fatal(err)
	}
	return row.Status
}

func (f *doneGuardFixture) rejectDone(story string, sender domain.SessionID, label string, wantInMessage string) {
	f.t.Helper()
	err := f.apply(story, "done", sender, "", "", "sha", "")
	if err == nil {
		f.t.Fatalf("%s: Done was accepted", label)
	}
	if wantInMessage != "" && !strings.Contains(err.Error(), wantInMessage) {
		f.t.Fatalf("%s: error %q does not mention %q", label, err, wantInMessage)
	}
	if got := f.status(story); got != "ready_for_approval" {
		f.t.Fatalf("%s: rejected Done changed the story to %s", label, got)
	}
}

func TestStoryDoneIsOnlyTheResponsibleManagers(t *testing.T) {
	f := newDoneGuardFixture(t)
	f.readyStory("managed", true)
	// The manager of ANOTHER repository's story.
	if err := f.svc.Create(f.ctx, domain.Story{ID: "elsewhere", EpicID: "e", Title: "TEST elsewhere", ProjectIDs: []domain.ProjectID{"repo", "repo2"}}); err != nil {
		t.Fatal(err)
	}
	f.must("elsewhere", "assignment", "", f.otherManager, "manager", "", "")

	// A worker or reviewer is pointed at ao report --done; the coordinator does not override a managed story.
	f.rejectDone("managed", f.worker, "worker", "ao report --done")
	f.rejectDone("managed", f.review, "reviewer", "ao report --done")
	f.rejectDone("managed", f.coordinator, "coordinator over a managed story", "assigned manager")
	f.rejectDone("managed", f.otherManager, "another story's manager", "must be assigned")

	// A stale revision is fenced before anything is decided.
	row, err := f.db.GetStory(f.ctx, "managed")
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.Apply(f.ctx, storysvc.Command{ID: "stale", StoryID: "managed", Revision: row.Revision - 1, Kind: "done", SenderSessionID: f.manager, Text: "Complete", Reference: "sha"})
	if err == nil || !strings.Contains(err.Error(), "Story changed") {
		t.Fatalf("stale revision accepted or wrong error: %v", err)
	}
	if got := f.status("managed"); got != "ready_for_approval" {
		t.Fatalf("story moved to %s", got)
	}

	// The responsible manager completes it after the review pass.
	f.must("managed", "done", f.manager, "", "", "sha", "")
	if got := f.status("managed"); got != "done" {
		t.Fatalf("manager Done left status %s", got)
	}
}

func TestStoryDoneStillNeedsIndependentReviewAtTheCurrentRevision(t *testing.T) {
	f := newDoneGuardFixture(t)
	if err := f.svc.Create(f.ctx, domain.Story{ID: "unreviewed", EpicID: "e", Title: "TEST unreviewed", ProjectIDs: []domain.ProjectID{"repo", "repo2"}}); err != nil {
		t.Fatal(err)
	}
	f.must("unreviewed", "assignment", "", f.worker, "worker", "", "")
	f.must("unreviewed", "assignment", "", f.review, "reviewer", "", "")
	f.must("unreviewed", "assignment", "", f.manager, "manager", "", "")
	f.must("unreviewed", "implementation", f.worker, "", "", "sha", "")
	f.must("unreviewed", "testing", f.worker, "", "", "sha", "passed")
	// No review at all, and a review of a different revision: neither lets even the manager complete it.
	if err := f.apply("unreviewed", "done", f.manager, "", "", "sha", ""); err == nil {
		t.Fatal("manager completed an unreviewed story")
	}
	f.must("unreviewed", "review_passed", f.review, "", "", "an-older-sha", "")
	if err := f.apply("unreviewed", "done", f.manager, "", "", "sha", ""); err == nil {
		t.Fatal("manager completed a story whose review is for another revision")
	}
	if got := f.status("unreviewed"); got == "done" {
		t.Fatal("story completed without a current review pass")
	}
}

func TestStoryDoneByATerminatedManagerIsRejected(t *testing.T) {
	f := newDoneGuardFixture(t)
	f.readyStory("stale-manager", true)
	manager, ok, err := f.db.GetSession(f.ctx, f.manager)
	if err != nil || !ok {
		t.Fatalf("manager session: %v %v", ok, err)
	}
	manager.IsTerminated = true
	if err := f.db.UpdateSession(f.ctx, manager); err != nil {
		t.Fatal(err)
	}
	f.rejectDone("stale-manager", f.manager, "terminated manager", "terminated")
	// The human board (no sender) is not an agent and still decides.
	f.must("stale-manager", "done", "", "", "", "sha", "")
	if got := f.status("stale-manager"); got != "done" {
		t.Fatalf("human Done left status %s", got)
	}
}

func TestStoryDoneWithoutAManagerIsTheCoordinatorsOrTheHumans(t *testing.T) {
	f := newDoneGuardFixture(t)
	f.readyStory("solo", false)
	f.rejectDone("solo", f.worker, "worker on a manager-less story", "ao report --done")
	f.rejectDone("solo", f.review, "reviewer on a manager-less story", "ao report --done")
	f.rejectDone("solo", f.otherManager, "unassigned manager", "must be assigned")
	f.must("solo", "done", f.coordinator, "", "", "sha", "")
	if got := f.status("solo"); got != "done" {
		t.Fatalf("coordinator Done left status %s", got)
	}
}
