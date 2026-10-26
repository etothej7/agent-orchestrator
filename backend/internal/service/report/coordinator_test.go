package report

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestCoordinatorFixedFallbackAndSettlementBatch(t *testing.T) {
	now := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	store := newCoordinatorStore(now)
	store.sessions = []domain.SessionRecord{{ID: "orch", ProjectID: "p", Kind: domain.KindOrchestrator, Mode: domain.SessionModeChat}}
	store.add(reportAt("one", "worker", now, now.Add(time.Hour), "first"))
	store.add(reportAt("two", "worker", now.Add(20*time.Minute), now.Add(80*time.Minute), "second"))
	delivery := &coordinatorDelivery{}
	c := NewCoordinator(CoordinatorDeps{Store: store, Delivery: delivery, Now: func() time.Time { return now }, NewToken: func() string { return "claim" }})

	if delay, err := c.RunDue(context.Background()); err != nil || delay != time.Hour {
		t.Fatalf("before fallback delay = %v, err = %v", delay, err)
	}
	now = now.Add(time.Hour)
	if _, err := c.RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(delivery.submissions) != 1 || !strings.Contains(delivery.submissions[0].message, "first") || !strings.Contains(delivery.submissions[0].message, "second") {
		t.Fatalf("submission = %#v, want one shared batch", delivery.submissions)
	}
	if store.reports[0].RecipientSessionID != "orch" {
		t.Fatalf("lost actual recipient: %+v", store.reports[0])
	}
	if store.acked != 2 {
		t.Fatalf("acknowledged = %d, want 2", store.acked)
	}

	store.add(reportAt("done-1", "worker", now, now.Add(5*time.Minute), "done"))
	store.add(reportAt("done-2", "worker", now.Add(2*time.Minute), now.Add(7*time.Minute), "later done"))
	now = now.Add(5 * time.Minute)
	if _, err := c.RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(delivery.submissions) != 2 || !strings.Contains(delivery.submissions[1].message, "later done") {
		t.Fatalf("settlement submission = %#v", delivery.submissions)
	}
}

func TestCoordinatorRoutesAtAttemptAndLeavesPendingWithoutOrchestrator(t *testing.T) {
	now := time.Date(2026, 9, 14, 11, 0, 0, 0, time.UTC)
	store := newCoordinatorStore(now)
	store.add(reportAt("need", "worker", now, now, "decision"))
	delivery := &coordinatorDelivery{}
	c := NewCoordinator(CoordinatorDeps{Store: store, Delivery: delivery, Now: func() time.Time { return now }, RetryDelay: time.Second})
	if delay, err := c.RunDue(context.Background()); err == nil || delay != time.Second {
		t.Fatalf("no orchestrator delay = %v, err = %v", delay, err)
	}
	if store.reports[0].DeliveryState != domain.ReportPending {
		t.Fatalf("state = %s, want pending", store.reports[0].DeliveryState)
	}
	store.sessions = []domain.SessionRecord{
		{ID: "old", ProjectID: "p", Kind: domain.KindOrchestrator, Mode: domain.SessionModeChat, IsTerminated: true, CreatedAt: now},
		{ID: "replacement", ProjectID: "p", Kind: domain.KindOrchestrator, Mode: domain.SessionModeChat, CreatedAt: now.Add(time.Second)},
	}
	if _, err := c.RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(delivery.submissions) != 1 || delivery.submissions[0].target != "replacement" {
		t.Fatalf("submissions = %#v", delivery.submissions)
	}
}

func TestCoordinatorStuckInterruptRateLimitAndCoalescing(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	store := newCoordinatorStore(now)
	store.sessions = []domain.SessionRecord{{ID: "orch", ProjectID: "p", Kind: domain.KindOrchestrator, Mode: domain.SessionModeChat}}
	first := reportAt("stuck-1", "worker", now, now, "cannot continue")
	first.State = domain.ReportStuck
	second := reportAt("stuck-2", "worker", now.Add(time.Second), now, "cannot continue")
	second.State = domain.ReportStuck
	store.add(first, second)
	delivery := &coordinatorDelivery{}
	c := NewCoordinator(CoordinatorDeps{Store: store, Delivery: delivery, Now: func() time.Time { return now }})
	if _, err := c.RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(delivery.interrupts) != 1 {
		t.Fatalf("interrupts = %v", delivery.interrupts)
	}
	if !strings.Contains(delivery.submissions[0].message, "repeated 2 times") {
		t.Fatalf("message = %q", delivery.submissions[0].message)
	}
	repeat := reportAt("stuck-3", "worker", now.Add(time.Minute), now, "cannot continue")
	repeat.State = domain.ReportStuck
	store.add(repeat)
	if _, err := c.RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(delivery.interrupts) != 1 {
		t.Fatalf("rate limited interrupts = %v", delivery.interrupts)
	}
	if len(delivery.submissions) != 1 {
		t.Fatalf("throttled repeat created an extra wakeup: %#v", delivery.submissions)
	}
}

func TestReportSessionLinkUsesDisplayName(t *testing.T) {
	got := reportSessionLink(domain.ReportRecord{
		SessionID: "worker/1", ProjectID: "proj 1", SessionDisplayName: "Backend [worker]",
	})
	want := `[Backend \[worker\]](ao://sessions/proj%201/worker%2F1)`
	if got != want {
		t.Fatalf("link = %q, want %q", got, want)
	}
}

func TestCoordinatorAcceptanceAndPiggybackFencing(t *testing.T) {
	now := time.Date(2026, 9, 14, 13, 0, 0, 0, time.UTC)
	store := newCoordinatorStore(now)
	store.sessions = []domain.SessionRecord{{ID: "orch", ProjectID: "p", Kind: domain.KindOrchestrator, Mode: domain.SessionModeChat}}
	store.add(reportAt("info", "worker", now, now.Add(time.Hour), "context"))
	c := NewCoordinator(CoordinatorDeps{Store: store, Delivery: &coordinatorDelivery{}, Now: func() time.Time { return now }, NewToken: func() string { return "piggyback-token" }})
	batch, err := c.PreparePiggyback(context.Background(), "orch")
	if err != nil || len(batch.Reports) != 1 {
		t.Fatalf("prepare = %#v, err = %v", batch, err)
	}
	combined := batch.AppendToUserMessage("hello")
	if !strings.HasPrefix(combined, "hello\n\n<ao-worker-reports>\n") ||
		!strings.HasSuffix(combined, "\n</ao-worker-reports>") {
		t.Fatalf("piggyback text = %q", combined)
	}
	if !strings.Contains(combined, "[checkpoint] [worker](ao://sessions/p/worker)") {
		t.Fatalf("piggyback link = %q", combined)
	}
	if store.acked != 0 {
		t.Fatal("claimed report acknowledged before acceptance")
	}
	if err := c.AcceptPiggyback(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	if store.acked != 1 {
		t.Fatalf("acknowledged = %d", store.acked)
	}
	if count, _ := store.AcknowledgeReportBatch(context.Background(), "p", "stale-token", now); count != 0 {
		t.Fatalf("stale token acknowledged %d reports", count)
	}
}

type coordinatorStore struct {
	mu         sync.Mutex
	now        time.Time
	reports    []domain.ReportRecord
	sessions   []domain.SessionRecord
	interrupts map[domain.SessionID]time.Time
	acked      int
}

func newCoordinatorStore(now time.Time) *coordinatorStore {
	return &coordinatorStore{now: now, interrupts: map[domain.SessionID]time.Time{}}
}

func reportAt(id string, worker domain.SessionID, created, available time.Time, note string) domain.ReportRecord {
	return domain.ReportRecord{ID: id, SessionID: worker, ProjectID: "p", State: domain.ReportCheckpoint, Note: note, CreatedAt: created, AvailableAt: available, DeliveryState: domain.ReportPending, RepeatCount: 1}
}

func (s *coordinatorStore) add(records ...domain.ReportRecord) {
	s.reports = append(s.reports, records...)
}
func (s *coordinatorStore) ListSessions(context.Context, domain.ProjectID) ([]domain.SessionRecord, error) {
	return append([]domain.SessionRecord(nil), s.sessions...), nil
}
func (s *coordinatorStore) GetSession(_ context.Context, id domain.SessionID) (domain.SessionRecord, bool, error) {
	for _, rec := range s.sessions {
		if rec.ID == id {
			return rec, true, nil
		}
	}
	return domain.SessionRecord{}, false, nil
}
func (s *coordinatorStore) ListPendingReportSchedule(context.Context, int64) ([]domain.ReportRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var pending []domain.ReportRecord
	for _, rec := range s.reports {
		if rec.DeliveryState == domain.ReportPending {
			pending = append(pending, rec)
		}
	}
	sortReports(pending)
	if len(pending) > 1 {
		pending = pending[:1]
	}
	return pending, nil
}
func (s *coordinatorStore) ClaimPendingReportBatch(_ context.Context, project domain.ProjectID, token string, at time.Time) ([]domain.ReportRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	batchID := ""
	for i := range s.reports {
		if s.reports[i].ProjectID == project && s.reports[i].DeliveryState == domain.ReportPending {
			batchID = s.reports[i].DeliveryBatchID
			if batchID == "" {
				batchID = "report-batch:" + s.reports[i].ID
			}
			break
		}
	}
	var claimed []domain.ReportRecord
	for i := range s.reports {
		if s.reports[i].ProjectID == project && s.reports[i].DeliveryState == domain.ReportPending {
			if s.reports[i].DeliveryBatchID != "" && s.reports[i].DeliveryBatchID != batchID {
				continue
			}
			s.reports[i].DeliveryBatchID = batchID
			s.reports[i].DeliveryState, s.reports[i].ClaimToken, s.reports[i].ClaimedAt = domain.ReportClaimed, token, at
			claimed = append(claimed, s.reports[i])
		}
	}
	sortReports(claimed)
	return claimed, nil
}
func (s *coordinatorStore) AcknowledgeReportBatch(_ context.Context, project domain.ProjectID, token string, at time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for i := range s.reports {
		if s.reports[i].ProjectID == project && s.reports[i].DeliveryState == domain.ReportClaimed && s.reports[i].ClaimToken == token {
			s.reports[i].DeliveryState, s.reports[i].AcknowledgedAt = domain.ReportAcknowledged, at
			count++
		}
	}
	s.acked += count
	return count, nil
}
func (s *coordinatorStore) ReleaseReportBatch(_ context.Context, project domain.ProjectID, token string, _ time.Time, _ string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for i := range s.reports {
		if s.reports[i].ProjectID == project && s.reports[i].DeliveryState == domain.ReportClaimed && s.reports[i].ClaimToken == token {
			s.reports[i].DeliveryState, s.reports[i].ClaimToken = domain.ReportPending, ""
			count++
		}
	}
	return count, nil
}
func (s *coordinatorStore) DeferReportBatch(ctx context.Context, project domain.ProjectID, token string, at time.Time, reason string) (int, error) {
	count, err := s.ReleaseReportBatch(ctx, project, token, at, reason)
	if err != nil {
		return count, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.reports {
		if s.reports[i].ProjectID == project && s.reports[i].DeliveryState == domain.ReportPending && s.reports[i].DeliveryBatchID != "" && s.reports[i].AvailableAt.Before(at) {
			s.reports[i].AvailableAt = at
		}
	}
	return count, nil
}
func (s *coordinatorStore) AcquireReportInterrupt(_ context.Context, worker domain.SessionID, at time.Time, window time.Duration) (bool, time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	last, ok := s.interrupts[worker]
	if ok && last.After(at.Add(-window)) {
		return false, last.Add(window), nil
	}
	s.interrupts[worker] = at
	return true, at, nil
}

func sortReports(records []domain.ReportRecord) {
	for i := 1; i < len(records); i++ {
		for j := i; j > 0 && (records[j].AvailableAt.Before(records[j-1].AvailableAt) || (records[j].AvailableAt.Equal(records[j-1].AvailableAt) && records[j].ID < records[j-1].ID)); j-- {
			records[j], records[j-1] = records[j-1], records[j]
		}
	}
}

type coordinatorSubmission struct {
	target       domain.SessionID
	message, key string
}
type coordinatorDelivery struct {
	submissions []coordinatorSubmission
	interrupts  []domain.SessionID
	submitErr   error
}

func (d *coordinatorDelivery) Submit(_ context.Context, target domain.SessionID, message, key string) error {
	if d.submitErr != nil {
		return d.submitErr
	}
	d.submissions = append(d.submissions, coordinatorSubmission{target, message, key})
	return nil
}
func (d *coordinatorDelivery) Interrupt(_ context.Context, target domain.SessionID) error {
	d.interrupts = append(d.interrupts, target)
	return nil
}

func (s *coordinatorStore) SetReportBatchRecipient(_ context.Context, project domain.ProjectID, token string, recipient domain.SessionID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.reports {
		if s.reports[i].DeliveryProjectID() == project && s.reports[i].ClaimToken == token {
			s.reports[i].RecipientSessionID = recipient
		}
	}
	return nil
}

func TestCoalesceInterleavedStoriesPreservesIdentity(t *testing.T) {
	first := domain.ReportRecord{ID: "report-a", SessionID: "shared-manager", ProjectID: "repo", StoryID: "story-a", State: domain.ReportStuck, Note: "Same blocker", RepeatCount: 1, Outputs: []domain.ReportOutput{{Kind: domain.ReportOutputArtifact, Reference: "same-result"}}}
	second := first
	second.ID, second.StoryID = "report-b", "story-b"
	repeat := first
	repeat.ID = "report-a-repeat"
	got := coalesceReports([]domain.ReportRecord{first, second, repeat})
	if len(got) != 2 || got[0].ID != first.ID || got[0].StoryID != first.StoryID || got[0].RepeatCount != 2 || got[1].ID != second.ID || got[1].StoryID != second.StoryID || got[1].RepeatCount != 1 {
		t.Fatalf("interleaved story identity lost: %+v", got)
	}
	rendered := (PreparedBatch{Reports: []domain.ReportRecord{first, second, repeat}}).Render()
	if strings.Count(rendered, "[story story-a]") != 1 || strings.Count(rendered, "[story story-b]") != 1 || strings.Count(rendered, "Same blocker") != 2 {
		t.Fatalf("delivery omitted a story: %s", rendered)
	}
}

type storyCoordinatorStore struct {
	*coordinatorStore
	stories map[string]domain.Story
}

func (s storyCoordinatorStore) GetStory(_ context.Context, id string) (domain.Story, error) {
	story, ok := s.stories[id]
	if !ok {
		return domain.Story{}, errors.New("story lookup failed")
	}
	return story, nil
}

func TestCoordinatorSkipsWorkerNotesForAlreadyDoneAutonomousStory(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	base := newCoordinatorStore(now)
	base.sessions = []domain.SessionRecord{{ID: "orch", ProjectID: "p", Kind: domain.KindOrchestrator, Mode: domain.SessionModeChat}}
	store := storyCoordinatorStore{coordinatorStore: base, stories: map[string]domain.Story{
		"finished": {ID: "finished", Autonomous: true, Status: "done"},
		"open":     {ID: "open", Autonomous: true, Status: "in_review"},
		"manual":   {ID: "manual", Status: "done"},
	}}
	story := func(id, storyID string, state domain.ReportState, note string) domain.ReportRecord {
		rec := reportAt(id, "worker", now, now, note)
		rec.State, rec.StoryID = state, storyID
		return rec
	}
	managerSummary := story("summary", "finished", domain.ReportDone, "manager summary")
	managerSummary.RecipientProjectID = "p"
	base.add(
		story("finished-done", "finished", domain.ReportDone, "worker done redundant"),
		story("finished-note", "finished", domain.ReportCheckpoint, "worker note redundant"),
		story("finished-question", "finished", domain.ReportNeedsInput, "still needs an answer"),
		story("finished-stuck", "finished", domain.ReportStuck, "blocked"),
		story("open-done", "open", domain.ReportDone, "open story result"),
		story("manual-done", "manual", domain.ReportDone, "manual story result"),
		story("unknown-done", "unlisted", domain.ReportDone, "lookup fails so deliver"),
		story("no-story", "", domain.ReportDone, "storyless result"),
		managerSummary,
	)
	delivery := &coordinatorDelivery{}
	c := NewCoordinator(CoordinatorDeps{Store: store, Delivery: delivery, Now: func() time.Time { return now }, NewToken: func() string { return "claim" }})
	if _, err := c.RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(delivery.submissions) != 1 {
		t.Fatalf("submissions = %#v, want one batch", delivery.submissions)
	}
	message := delivery.submissions[0].message
	for _, redundant := range []string{"worker done redundant", "worker note redundant"} {
		if strings.Contains(message, redundant) {
			t.Fatalf("redundant report delivered: %q in %q", redundant, message)
		}
	}
	for _, kept := range []string{"still needs an answer", "blocked", "open story result", "manual story result", "lookup fails so deliver", "storyless result", "manager summary"} {
		if !strings.Contains(message, kept) {
			t.Fatalf("report dropped: %q not in %q", kept, message)
		}
	}
	if base.acked != len(base.reports) {
		t.Fatalf("acknowledged = %d, want every claimed report (%d) settled", base.acked, len(base.reports))
	}
}

func TestCoordinatorAcknowledgesBatchOfOnlyRedundantStoryReportsWithoutWaking(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	base := newCoordinatorStore(now)
	base.sessions = []domain.SessionRecord{{ID: "orch", ProjectID: "p", Kind: domain.KindOrchestrator, Mode: domain.SessionModeChat}}
	store := storyCoordinatorStore{coordinatorStore: base, stories: map[string]domain.Story{"finished": {ID: "finished", Autonomous: true, Status: "done"}}}
	for _, state := range []domain.ReportState{domain.ReportDone, domain.ReportCheckpoint} {
		rec := reportAt("finished-"+string(state), "worker", now, now, "already recorded")
		rec.State, rec.StoryID = state, "finished"
		base.add(rec)
	}
	delivery := &coordinatorDelivery{}
	c := NewCoordinator(CoordinatorDeps{Store: store, Delivery: delivery, Now: func() time.Time { return now }, NewToken: func() string { return "claim" }})
	delay, err := c.RunDue(context.Background())
	if err != nil || delay >= 0 {
		t.Fatalf("delay = %v, err = %v; redundant reports must be fully settled", delay, err)
	}
	if len(delivery.submissions) != 0 || base.acked != 2 {
		t.Fatalf("submissions = %#v, acked = %d; want no manager wake and both acknowledged", delivery.submissions, base.acked)
	}
}
