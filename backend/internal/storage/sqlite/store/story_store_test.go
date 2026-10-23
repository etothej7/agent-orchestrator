package store_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	storysvc "github.com/aoagents/agent-orchestrator/backend/internal/service/story"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

type storyMessenger struct{ messages []ports.ChatUserMessage }

func (m *storyMessenger) SteerOrSend(_ context.Context, _ domain.SessionID, msg ports.ChatUserMessage, _ bool) (chatsvc.SteerOrSendResult, error) {
	m.messages = append(m.messages, msg)
	return chatsvc.SteerOrSendResult{Turn: domain.ConversationTurn{ID: "turn-" + msg.ClientMessageID, State: domain.TurnStateCompleted}}, nil
}

func TestStoryIsolationCompletionAndReopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	seedProject(t, db, "repo")
	seedProject(t, db, "repo2")
	seedProject(t, db, "coordinator")
	if err := db.SetCoordination(ctx, domain.CoordinationConfig{CoordinatorProjectID: "coordinator", ManagedProjectIDs: []domain.ProjectID{"repo", "repo2"}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	worker, err := db.CreateSession(ctx, sampleRecord("repo"))
	if err != nil {
		t.Fatal(err)
	}
	reviewer, err := db.CreateSession(ctx, sampleRecord("repo"))
	if err != nil {
		t.Fatal(err)
	}
	messenger := &storyMessenger{}
	svc := &storysvc.Service{Store: db, Messenger: messenger}
	for _, id := range []string{"one", "two"} {
		if err := svc.CreateEpic(ctx, domain.Epic{ID: id, Title: "TEST " + id}); err != nil {
			t.Fatal(err)
		}
		if err := svc.Create(ctx, domain.Story{ID: id, EpicID: id, Title: "TEST story " + id, ProjectIDs: []domain.ProjectID{"repo", "repo2"}}); err != nil {
			t.Fatal(err)
		}
	}
	seq := 0
	apply := func(story, kind string, sender, recipient domain.SessionID, role, ref, outcome string) (domain.StoryEvent, error) {
		t.Helper()
		row, err := db.GetStory(ctx, story)
		if err != nil {
			t.Fatal(err)
		}
		seq++
		return svc.Apply(ctx, storysvc.Command{ID: fmt.Sprintf("event-%d", seq), StoryID: story, Revision: row.Revision, Kind: kind, SenderSessionID: sender, RecipientSessionID: recipient, Role: role, Text: "Public explicit result", Reference: ref, Outcome: outcome})
	}
	for _, id := range []string{"one", "two"} {
		if _, err := apply(id, "assignment", "", worker.ID, "worker", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if len(messenger.messages) != 2 || messenger.messages[0].ClientMessageID == messenger.messages[1].ClientMessageID {
		t.Fatal("delivery handles crossed stories")
	}
	if _, err := apply("one", "assignment", "", reviewer.ID, "reviewer", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := apply("one", "done", "", "", "", "sha", ""); err == nil {
		t.Fatal("premature done accepted")
	}
	if _, err := apply("one", "implementation", reviewer.ID, "", "", "sha", ""); err == nil {
		t.Fatal("reviewer authored own implementation")
	}
	if _, err := apply("one", "implementation", worker.ID, "", "", "sha", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := apply("one", "testing", worker.ID, "", "", "sha", "failed"); err != nil {
		t.Fatal(err)
	}
	if _, err := apply("one", "review_passed", worker.ID, "", "", "sha", ""); err == nil {
		t.Fatal("self review accepted")
	}
	if _, err := apply("one", "review_passed", reviewer.ID, "", "", "sha", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := apply("one", "ready_for_approval", "", "", "", "", ""); err == nil {
		t.Fatal("failed checks accepted")
	}
	if _, err := apply("one", "testing", worker.ID, "", "", "sha", "passed"); err != nil {
		t.Fatal(err)
	}
	if _, err := apply("one", "ready_for_approval", "", "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := apply("one", "blocked", worker.ID, "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := apply("one", "done", "", "", "", "sha", ""); err == nil {
		t.Fatal("blocked story completed")
	}
	if _, err := apply("one", "unblocked", worker.ID, "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := apply("one", "done", "", "", "", "sha", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := apply("one", "testing", worker.ID, "", "", "sha", "failed"); err != nil {
		t.Fatal(err)
	}
	invalidated, err := db.GetStory(ctx, "one")
	if err != nil || invalidated.Status != "in_progress" {
		t.Fatalf("failed checks retained approval: %+v %v", invalidated, err)
	}
	for _, kind := range []string{"testing", "ready_for_approval", "done"} {
		if _, err := apply("one", kind, worker.ID, "", "", "sha", "passed"); err != nil {
			t.Fatal(err)
		}
	}
	for _, kind := range []string{"testing", "review_passed"} {
		actor := worker.ID
		if kind == "review_passed" {
			actor = reviewer.ID
		}
		if _, err := apply("one", kind, actor, "", "", "different-revision", "passed"); err != nil {
			t.Fatal(err)
		}
		row, err := db.GetStory(ctx, "one")
		if err != nil || row.Status != "in_progress" {
			t.Fatalf("incompatible passing evidence retained approval: %+v %v", row, err)
		}
		if _, err := apply("one", kind, actor, "", "", "sha", "passed"); err != nil {
			t.Fatal(err)
		}
		if _, err := apply("one", "ready_for_approval", "", "", "", "", ""); err != nil {
			t.Fatal(err)
		}
		if _, err := apply("one", "done", "", "", "", "sha", ""); err != nil {
			t.Fatal(err)
		}
	}
	// A worker's done report remains a claim and does not complete its other story.
	now := time.Now().UTC()
	report := domain.ReportRecord{ID: "rpt_story", SessionID: worker.ID, ProjectID: "repo", StoryID: "two", ClientReportID: "report-key", State: domain.ReportDone, Note: "Completed worker scope", CreatedAt: now, DeliveryState: domain.ReportPending, AvailableAt: now.Add(domain.ReportSettlementWindow), SettlementDeadline: now.Add(domain.ReportSettlementWindow), RepeatCount: 1}
	if _, err := db.CreateReport(ctx, report); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateReport(ctx, report); err != nil {
		t.Fatal(err)
	}
	report.StoryID = "one"
	if _, err := db.CreateReport(ctx, report); err == nil {
		t.Fatal("report key leaked to another story")
	}
	if _, err := db.ClaimPendingReportBatch(ctx, "repo", "delivery-token", now.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := db.SetReportBatchRecipient(ctx, "repo", "delivery-token", reviewer.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcknowledgeReportBatch(ctx, "repo", "delivery-token", now.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	delivered, found, err := db.GetReport(ctx, "rpt_story")
	if err != nil || !found || delivered.RecipientSessionID != reviewer.ID || delivered.DeliveryState != domain.ReportAcknowledged {
		t.Fatalf("lost durable report recipient: %+v %v", delivered, err)
	}
	one, err := db.GetStory(ctx, "one")
	if err != nil || one.Status != "done" {
		t.Fatalf("one=%+v err=%v", one, err)
	}
	two, err := db.GetStory(ctx, "two")
	if err != nil || two.Status != "backlog" {
		t.Fatalf("report advanced story: %+v %v", two, err)
	}
	events, err := db.StoryEvents(ctx, "two")
	if err != nil || len(events) != 2 || events[1].Reference != "rpt_story" {
		t.Fatalf("events=%+v err=%v", events, err)
	}
}

func TestStoryCommandRetryAndRevisionFence(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)
	if err := db.CreateEpic(ctx, domain.Epic{ID: "e", Title: "TEST"}); err != nil {
		t.Fatal(err)
	}
	story := domain.Story{ID: "s", EpicID: "e", Title: "TEST", Status: "backlog", Revision: 1}
	if err := db.CreateStory(ctx, story); err != nil {
		t.Fatal(err)
	}
	svc := &storysvc.Service{Store: db}
	cmd := storysvc.Command{ID: "k", StoryID: "s", Revision: 1, Kind: "blocked", Text: "Need Ed's decision"}
	if _, err := svc.Apply(ctx, cmd); err != nil {
		t.Fatal(err)
	}
	cmd.Revision = 2
	if _, err := svc.Apply(ctx, cmd); err != nil {
		t.Fatal(err)
	}
	cmd.Text = "different"
	if _, err := svc.Apply(ctx, cmd); err == nil {
		t.Fatal("key reuse accepted")
	}
	cmd.ID = "other"
	cmd.Revision = 1
	if _, err := svc.Apply(ctx, cmd); err == nil {
		t.Fatal("stale edit accepted")
	}
	events, err := db.StoryEvents(ctx, "s")
	if err != nil || len(events) != 1 {
		t.Fatalf("duplicate events: %+v %v", events, err)
	}
}

func TestNativeReviewRemainsLinkedToItsStory(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)
	seedProject(t, db, "repo")
	worker, err := db.CreateSession(ctx, sampleRecord("repo"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateEpic(ctx, domain.Epic{ID: "e", Title: "TEST"}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateStory(ctx, domain.Story{ID: "s", EpicID: "e", Title: "TEST", Status: "in_review", Revision: 1}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := db.UpsertReview(ctx, domain.Review{ID: "review", SessionID: worker.ID, ProjectID: "repo", Harness: domain.ReviewerCodex, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	run := domain.ReviewRun{ID: "run", ReviewID: "review", SessionID: worker.ID, Harness: domain.ReviewerCodex, PRURL: "https://github.com/test/repo/pull/1", TargetSHA: "fixed-sha", Status: domain.ReviewRunRunning, CreatedAt: now, StoryID: "s"}
	if err := db.InsertReviewRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpdateReviewRunResult(ctx, "run", domain.ReviewRunComplete, domain.VerdictApproved, "Public independent findings", "", false); err != nil {
		t.Fatal(err)
	}
	story, err := db.GetStory(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	story.Status = "ready_for_approval"
	if _, _, err := db.ApplyStoryEvent(ctx, story, domain.StoryEvent{ID: "approve", StoryID: "s", Kind: "ready_for_approval"}, "{}"); err != nil {
		t.Fatal(err)
	}
	run.ID = "second-run"
	if err := db.InsertReviewRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpdateReviewRunResult(ctx, "second-run", domain.ReviewRunComplete, domain.VerdictChangesRequested, "New independent findings", "", false); err != nil {
		t.Fatal(err)
	}
	story, err = db.GetStory(ctx, "s")
	if err != nil || story.Status != "in_progress" {
		t.Fatalf("failed native review retained approval: %+v %v", story, err)
	}
	got, ok, err := db.GetReviewRun(ctx, "run")
	if err != nil || !ok || got.StoryID != "s" {
		t.Fatalf("run=%+v ok=%v err=%v", got, ok, err)
	}
	events, err := db.StoryEvents(ctx, "s")
	if err != nil || len(events) != 3 || events[0].Result != "approved" || events[0].Reference != "fixed-sha" || events[0].Text != "Public independent findings" {
		t.Fatalf("events=%+v err=%v", events, err)
	}
}

type controlledStoryMessenger struct {
	result     chatsvc.SteerOrSendResult
	err        error
	recipients []domain.SessionID
	keys       []string
}

func (m *controlledStoryMessenger) SteerOrSend(_ context.Context, id domain.SessionID, msg ports.ChatUserMessage, _ bool) (chatsvc.SteerOrSendResult, error) {
	m.recipients = append(m.recipients, id)
	m.keys = append(m.keys, msg.ClientMessageID)
	return m.result, m.err
}

func TestStoryAnswerRoutesToRequesterAndKeepsQuestionUntilDelivered(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)
	seedProject(t, db, "repo")
	requester, err := db.CreateSession(ctx, sampleRecord("repo"))
	if err != nil {
		t.Fatal(err)
	}
	if err = db.CreateEpic(ctx, domain.Epic{ID: "e", Title: "TEST"}); err != nil {
		t.Fatal(err)
	}
	if err = db.CreateStory(ctx, domain.Story{ID: "s", EpicID: "e", Title: "TEST", Status: "backlog", Revision: 1, Assignments: []domain.StoryAssignment{{SessionID: requester.ID, Role: "manager"}}}); err != nil {
		t.Fatal(err)
	}
	m := &controlledStoryMessenger{result: chatsvc.SteerOrSendResult{Turn: domain.ConversationTurn{ID: "queued-turn", State: domain.TurnStateQueued}}}
	svc := &storysvc.Service{Store: db, Messenger: m}
	if _, err = svc.Apply(ctx, storysvc.Command{ID: "question", StoryID: "s", Revision: 1, Kind: "question", SenderSessionID: requester.ID, Text: "Which color?"}); err != nil {
		t.Fatal(err)
	}
	answer := storysvc.Command{ID: "answer", StoryID: "s", Revision: 2, Kind: "answer", Text: "Blue"}
	event, err := svc.Apply(ctx, answer)
	if err != nil || event.Result != "queued" {
		t.Fatalf("queued answer=%+v %v", event, err)
	}
	row, _ := db.GetStory(ctx, "s")
	if row.Question == "" || row.QuestionRequesterSessionID != requester.ID {
		t.Fatalf("question lost before delivery: %+v", row)
	}
	m.result = chatsvc.SteerOrSendResult{Turn: domain.ConversationTurn{ID: "queued-turn", State: domain.TurnStateRunning, ProviderTurnID: "private-provider-id"}}
	event, err = svc.Apply(ctx, answer)
	if err != nil || event.Result != "delivered" {
		t.Fatalf("delivered answer=%+v %v", event, err)
	}
	row, _ = db.GetStory(ctx, "s")
	if row.Question != "" {
		t.Fatalf("delivered answer unresolved: %+v", row)
	}
	if len(m.keys) != 2 || m.keys[0] != m.keys[1] || m.recipients[0] != requester.ID || m.recipients[1] != requester.ID {
		t.Fatalf("wrong target or retry key: %+v", m)
	}

	row, _ = db.GetStory(ctx, "s")
	if _, err = svc.Apply(ctx, storysvc.Command{ID: "question-two", StoryID: "s", Revision: row.Revision, Kind: "question", SenderSessionID: requester.ID, Text: "Second decision?"}); err != nil {
		t.Fatal(err)
	}
	row, _ = db.GetStory(ctx, "s")
	m.err = chatsvc.ErrNotChatMode
	event, err = svc.Apply(ctx, storysvc.Command{ID: "failed-answer", StoryID: "s", Revision: row.Revision, Kind: "answer", Text: "Cannot deliver"})
	if err == nil || event.Result != "failed" {
		t.Fatalf("failed answer=%+v %v", event, err)
	}
	row, _ = db.GetStory(ctx, "s")
	if row.Question != "Second decision?" {
		t.Fatal("failed answer cleared unresolved question")
	}
}

func TestStoryDeliveryDefinitiveFailureIsNotUncertain(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{{"unsupported", chatsvc.ErrNotChatMode, "failed"}, {"uncertain", chatsvc.ErrSteerDeliveryUncertain, "uncertain"}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			db := newTestStore(t)
			seedProject(t, db, "repo")
			recipient, err := db.CreateSession(ctx, sampleRecord("repo"))
			if err != nil {
				t.Fatal(err)
			}
			if err = db.CreateEpic(ctx, domain.Epic{ID: "e", Title: "TEST"}); err != nil {
				t.Fatal(err)
			}
			if err = db.CreateStory(ctx, domain.Story{ID: "s", EpicID: "e", Title: "TEST", Status: "backlog", Revision: 1, Assignments: []domain.StoryAssignment{{SessionID: recipient.ID, Role: "manager"}}}); err != nil {
				t.Fatal(err)
			}
			m := &controlledStoryMessenger{err: tc.err}
			svc := &storysvc.Service{Store: db, Messenger: m}
			cmd := storysvc.Command{ID: "send", StoryID: "s", Revision: 1, Kind: "message", RecipientSessionID: recipient.ID, Text: "Question"}
			event, err := svc.Apply(ctx, cmd)
			if err == nil || event.Result != tc.want {
				t.Fatalf("result=%+v %v", event, err)
			}
			_, _ = svc.Apply(ctx, cmd)
			wantCalls := 2
			if tc.want == "failed" {
				wantCalls = 1
			}
			if len(m.keys) != wantCalls {
				t.Fatalf("unexpected redelivery count %d", len(m.keys))
			}
		})
	}
}

func TestStoryLateAnswerCannotResolveNewerQuestion(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)
	seedProject(t, db, "repo")
	requester, err := db.CreateSession(ctx, sampleRecord("repo"))
	if err != nil {
		t.Fatal(err)
	}
	if err = db.CreateEpic(ctx, domain.Epic{ID: "e", Title: "TEST"}); err != nil {
		t.Fatal(err)
	}
	if err = db.CreateStory(ctx, domain.Story{ID: "s", EpicID: "e", Title: "TEST", Status: "backlog", Revision: 1, Assignments: []domain.StoryAssignment{{SessionID: requester.ID, Role: "manager"}}}); err != nil {
		t.Fatal(err)
	}
	m := &controlledStoryMessenger{result: chatsvc.SteerOrSendResult{Turn: domain.ConversationTurn{ID: "turn", State: domain.TurnStateQueued}}}
	svc := &storysvc.Service{Store: db, Messenger: m}
	if _, err = svc.Apply(ctx, storysvc.Command{ID: "first-question", StoryID: "s", Revision: 1, Kind: "question", SenderSessionID: requester.ID, Text: "First?"}); err != nil {
		t.Fatal(err)
	}
	answer := storysvc.Command{ID: "late-answer", StoryID: "s", Revision: 2, Kind: "answer", Text: "First answer"}
	if _, err = svc.Apply(ctx, answer); err != nil {
		t.Fatal(err)
	}
	row, _ := db.GetStory(ctx, "s")
	if _, err = svc.Apply(ctx, storysvc.Command{ID: "new-question", StoryID: "s", Revision: row.Revision, Kind: "question", SenderSessionID: requester.ID, Text: "New question?"}); err != nil {
		t.Fatal(err)
	}
	m.result.Turn.State = domain.TurnStateCompleted
	if _, err = svc.Apply(ctx, answer); err != nil {
		t.Fatal(err)
	}
	row, _ = db.GetStory(ctx, "s")
	if row.Question != "New question?" || row.QuestionEventID != "new-question" {
		t.Fatalf("late answer cleared a different question: %+v", row)
	}
}
