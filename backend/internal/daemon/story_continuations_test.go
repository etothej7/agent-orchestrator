package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/cdc"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	reportsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/report"
	storysvc "github.com/aoagents/agent-orchestrator/backend/internal/service/story"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

type continuationSweepProbe struct {
	storysvc.Store
	called chan struct{}
}

func (s *continuationSweepProbe) ListStories(context.Context) ([]domain.Story, error) {
	select {
	case s.called <- struct{}{}:
	default:
	}
	return nil, nil
}
func TestStoryContinuationsWaitForRecoveryAndUnsubscribeOnShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	recovered := make(chan struct{})
	events := cdc.NewBroadcaster()
	probe := &continuationSweepProbe{called: make(chan struct{}, 4)}
	runner := &storysvc.Continuations{Stories: &storysvc.Service{Store: probe}}
	done := startStoryContinuations(ctx, recovered, events, runner, slog.New(slog.DiscardHandler))
	select {
	case <-probe.called:
		t.Fatal("dispatch ran before provider recovery")
	case <-time.After(30 * time.Millisecond):
	}
	close(recovered)
	select {
	case <-probe.called:
	case <-time.After(time.Second):
		t.Fatal("startup did not reconcile")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("continuation loop did not stop")
	}
	if events.SubscriberCount() != 0 {
		t.Fatal("continuation subscription leaked")
	}
}

// The dispatch boundary persists a real automation turn; all eligibility,
// revision fencing, polling and subscriptions use the production SQLite path.
type phaseTurnSender struct {
	db   *sqlite.Store
	sent chan ports.ChatUserMessage
}

func (s *phaseTurnSender) Send(ctx context.Context, id domain.SessionID, msg ports.ChatUserMessage) (domain.ConversationTurn, error) {
	now := time.Now().UTC()
	_, err := s.db.AppendUserMessage(ctx, "phase-thread", id, "phase-generation", domain.ConversationMessage{ID: msg.ClientMessageID, ClientMessageID: msg.ClientMessageID, Text: msg.Text, Origin: domain.MessageOriginAutomation}, "turn-"+msg.ClientMessageID, now)
	if err != nil {
		return domain.ConversationTurn{}, err
	}
	rec, _, err := s.db.GetSession(ctx, id)
	if err != nil {
		return domain.ConversationTurn{}, err
	}
	rec.Activity.State = domain.ActivityActive
	if err = s.db.UpdateSession(ctx, rec); err != nil {
		return domain.ConversationTurn{}, err
	}
	turn, err := s.db.TurnByID(ctx, "turn-"+msg.ClientMessageID)
	s.sent <- msg
	return turn, err
}
func TestPhaseStatusCDCDispatchesWithoutHumanWake(t *testing.T) {
	for _, status := range []string{"in_review", "ready_for_approval", "done"} {
		t.Run(status, func(t *testing.T) {
			db, err := sqlite.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			now := time.Now().UTC()
			for _, id := range []string{"repo", "coordinator"} {
				if err = db.UpsertProject(ctx, domain.ProjectRecord{ID: id, Path: t.TempDir(), RegisteredAt: now}); err != nil {
					t.Fatal(err)
				}
			}
			if err = db.SetCoordination(ctx, domain.CoordinationConfig{CoordinatorProjectID: "coordinator", ManagedProjectIDs: []domain.ProjectID{"repo"}}, now); err != nil {
				t.Fatal(err)
			}
			manager, err := db.CreateSession(ctx, domain.SessionRecord{ProjectID: "repo", Kind: domain.KindOrchestrator, Harness: domain.HarnessCodex, Mode: domain.SessionModeChat, Activity: domain.Activity{State: domain.ActivityActive, LastActivityAt: now}, CreatedAt: now, UpdatedAt: now})
			if err != nil {
				t.Fatal(err)
			}
			_, err = db.CreateSession(ctx, domain.SessionRecord{ProjectID: "coordinator", Kind: domain.KindOrchestrator, Harness: domain.HarnessCodex, CreatedAt: now, UpdatedAt: now})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.CreateConversation(ctx, "phase-thread", domain.ConversationScopeSession, "repo", manager.ID, now); err != nil {
				t.Fatal(err)
			}
			if err = db.CreateEpic(ctx, domain.Epic{ID: "epic", Title: "TEST phase wake"}); err != nil {
				t.Fatal(err)
			}
			source := domain.Story{ID: "source", EpicID: "epic", Title: "TEST phase", Autonomous: true, Status: "in_progress", Revision: 1, ProjectIDs: []domain.ProjectID{"repo"}, Assignments: []domain.StoryAssignment{{SessionID: manager.ID, Role: "manager"}}}
			if err = db.CreateStory(ctx, source); err != nil {
				t.Fatal(err)
			}
			target := "source"
			if status == "done" {
				next := source
				next.ID = "next"
				next.Status = "backlog"
				if err = db.CreateStory(ctx, next); err != nil {
					t.Fatal(err)
				}
				target = "next"
			}
			sender := &phaseTurnSender{db: db, sent: make(chan ports.ChatUserMessage, 4)}
			events := cdc.NewBroadcaster()
			poller := cdc.NewPoller(db, events, cdc.PollerConfig{Interval: 10 * time.Millisecond})
			if err = poller.SeekToHead(ctx); err != nil {
				t.Fatal(err)
			}
			pollDone := poller.Start(ctx)
			recovered := make(chan struct{})
			close(recovered)
			runner := &storysvc.Continuations{Store: db, Stories: &storysvc.Service{Store: db}, Sender: sender}
			done := startStoryContinuations(ctx, recovered, events, runner, slog.New(slog.DiscardHandler))
			defer func() { cancel(); <-done; <-pollDone }()
			deadline := time.Now().Add(2 * time.Second)
			for {
				rec, found, e := db.GetStoryContinuation(ctx, target)
				if e != nil {
					t.Fatal(e)
				}
				if found && !rec.WaitingSince.IsZero() && rec.NextAt.After(time.Now()) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("busy-owner backoff not established", rec)
				}
				time.Sleep(5 * time.Millisecond)
			}
			started := time.Now()
			// Seed only a status fact to exercise scheduler wiring, not review validity.
			source.Status = status
			if _, _, err = db.ApplyStoryEvent(ctx, source, domain.StoryEvent{ID: "phase-event", StoryID: source.ID, Kind: "status", Text: "TEST phase transition", CreatedAt: now}, `{ "operation": "phase-transition" }`); err != nil {
				t.Fatal(err)
			}
			manager.Activity.State = domain.ActivityIdle
			if err = db.UpdateSession(ctx, manager); err != nil {
				t.Fatal(err)
			}
			var delivered ports.ChatUserMessage
			select {
			case msg := <-sender.sent:
				delivered = msg
				if !strings.Contains(msg.Text, "Story "+target+";") {
					t.Fatal("wrong next story", msg.Text)
				}
				t.Logf("%s CDC -> durable next dispatch %s in %s", status, msg.ClientMessageID, time.Since(started))
			case <-time.After(2 * time.Second):
				t.Fatal("phase waited for timer/human instead of CDC")
			}

			if status == "in_review" {
				var generation int64
				for attempt := 1; attempt <= 3; attempt++ {
					deadline := time.Now().Add(2 * time.Second)
					for {
						rec, _, e := db.GetStoryContinuation(ctx, target)
						if e != nil {
							t.Fatal(e)
						}
						if rec.TurnID == "turn-"+delivered.ClientMessageID {
							if attempt == 1 {
								generation = rec.Generation
							}
							if rec.Generation != generation || rec.ProgressAttempts != attempt {
								t.Fatal("status reset progress budget", rec)
							}
							break
						}
						if time.Now().After(deadline) {
							t.Fatal("dispatch not settled", rec)
						}
						time.Sleep(5 * time.Millisecond)
					}
					if err = db.SettleTurnByID(ctx, "turn-"+delivered.ClientMessageID, domain.TurnStateCompleted, "", time.Now()); err != nil {
						t.Fatal(err)
					}
					source, err = db.GetStory(ctx, "source")
					if err != nil {
						t.Fatal(err)
					}
					source.Status = "in_progress"
					if attempt%2 == 0 {
						source.Status = "in_review"
					}
					if _, _, err = db.ApplyStoryEvent(ctx, source, domain.StoryEvent{ID: fmt.Sprintf("oscillate-%d", attempt), StoryID: source.ID, Kind: "status", CreatedAt: time.Now()}, `{}`); err != nil {
						t.Fatal(err)
					}
					manager.Activity.State = domain.ActivityIdle
					if err = db.UpdateSession(ctx, manager); err != nil {
						t.Fatal(err)
					}
					if attempt < 3 {
						select {
						case delivered = <-sender.sent:
						case <-time.After(2 * time.Second):
							t.Fatal("settled status did not wake promptly")
						}
					} else {
						deadline = time.Now().Add(2 * time.Second)
						for {
							rec, _, e := db.GetStoryContinuation(ctx, target)
							if e != nil {
								t.Fatal(e)
							}
							if rec.State == "blocked" {
								if rec.ProgressAttempts != 3 || rec.Generation != generation {
									t.Fatal(rec)
								}
								break
							}
							if time.Now().After(deadline) {
								t.Fatal("status oscillation escaped bound", rec)
							}
							time.Sleep(5 * time.Millisecond)
						}
						select {
						case extra := <-sender.sent:
							t.Fatal("fourth dispatch escaped bound", extra.ClientMessageID)
						default:
						}
					}
				}
			}
		})
	}
}

// No report coordinator is running: only the production story-event CDC runner
// can deliver this next manager turn. The normal report outbox remains pending.
func TestAutonomousPhaseReportCDCBypassesSettlement(t *testing.T) {
	for _, scenario := range []string{"worker", "reviewer", "busy-worker", "active-worker"} {
		busy := scenario == "busy-worker"
		role := strings.TrimPrefix(strings.TrimPrefix(scenario, "busy-"), "active-")
		for _, state := range []domain.ReportState{domain.ReportDone, domain.ReportNeedsInput, domain.ReportStuck} {
			if (busy || scenario == "active-worker") && state != domain.ReportDone {
				continue
			}
			t.Run(scenario+"/"+string(state), func(t *testing.T) {
				db, err := sqlite.Open(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				now := time.Now().UTC()
				for _, id := range []string{"repo", "coordinator"} {
					if err = db.UpsertProject(ctx, domain.ProjectRecord{ID: id, Path: t.TempDir(), RegisteredAt: now}); err != nil {
						t.Fatal(err)
					}
				}
				if err = db.SetCoordination(ctx, domain.CoordinationConfig{CoordinatorProjectID: "coordinator", ManagedProjectIDs: []domain.ProjectID{"repo"}}, now); err != nil {
					t.Fatal(err)
				}
				makeSession := func(project domain.ProjectID, kind domain.SessionKind) domain.SessionRecord {
					rec, e := db.CreateSession(ctx, domain.SessionRecord{ProjectID: project, Kind: kind, Harness: domain.HarnessCodex, Mode: domain.SessionModeChat, Activity: domain.Activity{State: domain.ActivityIdle, LastActivityAt: now}, CreatedAt: now, UpdatedAt: now})
					if e != nil {
						t.Fatal(e)
					}
					return rec
				}
				manager := makeSession("repo", domain.KindOrchestrator)
				reporter := makeSession("repo", domain.KindWorker)
				makeSession("coordinator", domain.KindOrchestrator)
				if _, err = db.CreateConversation(ctx, "phase-thread", domain.ConversationScopeSession, "repo", manager.ID, now); err != nil {
					t.Fatal(err)
				}
				if err = db.CreateEpic(ctx, domain.Epic{ID: "epic", Title: "TEST phase report"}); err != nil {
					t.Fatal(err)
				}
				story := domain.Story{ID: "story", EpicID: "epic", Title: "TEST report", Autonomous: true, Status: "in_progress", Revision: 1, ProjectIDs: []domain.ProjectID{"repo"}, Assignments: []domain.StoryAssignment{{SessionID: manager.ID, Role: "manager"}, {SessionID: reporter.ID, Role: role}}}
				if err = db.CreateStory(ctx, story); err != nil {
					t.Fatal(err)
				}
				other := story
				other.ID = "other"
				other.Autonomous = false
				if err = db.CreateStory(ctx, other); err != nil {
					t.Fatal(err)
				}
				sender := &phaseTurnSender{db: db, sent: make(chan ports.ChatUserMessage, 4)}
				events := cdc.NewBroadcaster()
				poller := cdc.NewPoller(db, events, cdc.PollerConfig{Interval: 10 * time.Millisecond})
				if err = poller.SeekToHead(ctx); err != nil {
					t.Fatal(err)
				}
				pollDone := poller.Start(ctx)
				recovered := make(chan struct{})
				close(recovered)
				runner := &storysvc.Continuations{Store: db, Stories: &storysvc.Service{Store: db}, Sender: sender}
				done := startStoryContinuations(ctx, recovered, events, runner, slog.New(slog.DiscardHandler))
				defer func() { cancel(); <-done; <-pollDone }()
				var first ports.ChatUserMessage
				select {
				case first = <-sender.sent:
				case <-time.After(2 * time.Second):
					t.Fatal("initial manager dispatch missing")
				}
				waitReceipt := func(key string) domain.StoryContinuation {
					deadline := time.Now().Add(2 * time.Second)
					for {
						rec, _, e := db.GetStoryContinuation(ctx, story.ID)
						if e != nil {
							t.Fatal(e)
						}
						if rec.TurnID == "turn-"+key {
							return rec
						}
						if time.Now().After(deadline) {
							t.Fatal("receipt missing", rec)
						}
						time.Sleep(5 * time.Millisecond)
					}
				}
				before := waitReceipt(first.ClientMessageID)
				finishManager := func() {
					if err = db.SettleTurnByID(ctx, before.TurnID, domain.TurnStateCompleted, "", time.Now()); err != nil {
						t.Fatal(err)
					}
					manager.Activity.State = domain.ActivityIdle
					if err = db.UpdateSession(ctx, manager); err != nil {
						t.Fatal(err)
					}
				}
				if !busy {
					finishManager()
				}
				reports := reportsvc.New(reportsvc.Deps{Store: db})
				checkpoint, err := reports.Create(ctx, reportsvc.CreateInput{SessionID: reporter.ID, StoryID: story.ID, ClientReportID: "checkpoint", State: domain.ReportCheckpoint, Note: "informational"})
				if err != nil {
					t.Fatal(err)
				}
				if !checkpoint.AvailableAt.After(time.Now().Add(50 * time.Minute)) {
					t.Fatal("checkpoint batching changed", checkpoint)
				}
				if _, err = reports.Create(ctx, reportsvc.CreateInput{SessionID: reporter.ID, StoryID: other.ID, ClientReportID: "other-phase", State: state, Note: "another story"}); err != nil {
					t.Fatal(err)
				}
				select {
				case msg := <-sender.sent:
					t.Fatal("checkpoint/unrelated story bypassed backoff", msg.ClientMessageID)
				case <-time.After(100 * time.Millisecond):
				}
				if scenario == "active-worker" {
					reporter.Activity.State = domain.ActivityActive
					if err = db.UpdateSession(ctx, reporter); err != nil {
						t.Fatal(err)
					}
				}
				started := time.Now()
				input := reportsvc.CreateInput{SessionID: reporter.ID, StoryID: story.ID, ClientReportID: "phase-report", State: state, Note: "actual phase result"}
				report, err := reports.Create(ctx, input)
				if err != nil {
					t.Fatal(err)
				}
				if state == domain.ReportDone && !report.AvailableAt.After(started.Add(4*time.Minute)) {
					t.Fatal("human-facing settlement was bypassed", report)
				}
				eligibleAt := started
				if busy {
					select {
					case msg := <-sender.sent:
						t.Fatal("busy receipt was bypassed", msg.ClientMessageID)
					case <-time.After(50 * time.Millisecond):
					}
					waiting, _, e := db.GetStoryContinuation(ctx, story.ID)
					if e != nil {
						t.Fatal(e)
					}
					if waiting.TurnID != before.TurnID || waiting.PhaseReportID != "" {
						t.Fatal("busy receipt/report wake lost", waiting)
					}
					eligibleAt = time.Now()
					finishManager()
				}
				var next ports.ChatUserMessage
				select {
				case next = <-sender.sent:
				case <-time.After(2 * time.Second):
					t.Fatal("story report CDC waited for settlement/human")
				}
				dispatchedAt := time.Now()
				after := waitReceipt(next.ClientMessageID)
				if after.Generation != before.Generation || after.ProgressAttempts != before.ProgressAttempts+1 || after.PhaseReportID != "report-"+report.ID {
					t.Fatal("report reset progress budget or lost durable identity", before, after)
				}
				if next.ClientMessageID == first.ClientMessageID || !strings.Contains(next.Text, "Story story;") {
					t.Fatal("wrong/new dispatch identity", next)
				}
				persisted, found, err := db.GetReport(ctx, report.ID)
				if err != nil || !found || persisted.DeliveryState != domain.ReportPending || persisted.DeliveryAttempts != 0 {
					t.Fatal("dispatch depended on outbox consumption", persisted, err)
				}
				replay, err := reports.Create(ctx, input)
				if err != nil || replay.ID != report.ID {
					t.Fatal("report retry changed identity", replay, err)
				}
				current, err := db.GetStory(ctx, story.ID)
				if err != nil || current.Question != "" || current.Status != "in_progress" {
					t.Fatal("report inferred human question/completion", current, err)
				}
				select {
				case extra := <-sender.sent:
					t.Fatal("duplicate report dispatched again", extra.ClientMessageID)
				case <-time.After(50 * time.Millisecond):
				}
				t.Logf("%s %s report %s -> manager %s turn %s via CDC in %s after eligibility (%s since report); outbox remains pending", scenario, state, report.ID, manager.ID, after.TurnID, dispatchedAt.Sub(eligibleAt), dispatchedAt.Sub(started))
			})
		}
	}
}

// A freshly queued manager turn is durable before its provider reports the
// session active. The first assignment turn already advances the checkpoint, so
// a continuation sent in that window only queues a redundant turn behind it.
func TestStoryContinuationWaitsForManagersOwnOpenTurn(t *testing.T) {
	db, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	now := time.Now().UTC()
	for _, id := range []string{"repo", "coordinator"} {
		if err = db.UpsertProject(ctx, domain.ProjectRecord{ID: id, Path: t.TempDir(), RegisteredAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	if err = db.SetCoordination(ctx, domain.CoordinationConfig{CoordinatorProjectID: "coordinator", ManagedProjectIDs: []domain.ProjectID{"repo"}}, now); err != nil {
		t.Fatal(err)
	}
	manager, err := db.CreateSession(ctx, domain.SessionRecord{ProjectID: "repo", Kind: domain.KindOrchestrator, Harness: domain.HarnessCodex, Mode: domain.SessionModeChat, Activity: domain.Activity{State: domain.ActivityIdle, LastActivityAt: now}, CreatedAt: now, UpdatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.CreateSession(ctx, domain.SessionRecord{ProjectID: "coordinator", Kind: domain.KindOrchestrator, Harness: domain.HarnessCodex, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err = db.CreateConversation(ctx, "phase-thread", domain.ConversationScopeSession, "repo", manager.ID, now); err != nil {
		t.Fatal(err)
	}
	if err = db.CreateEpic(ctx, domain.Epic{ID: "epic", Title: "TEST open turn"}); err != nil {
		t.Fatal(err)
	}
	if err = db.CreateStory(ctx, domain.Story{ID: "story", EpicID: "epic", Title: "TEST open turn", Autonomous: true, Status: "backlog", Revision: 1, ProjectIDs: []domain.ProjectID{"repo"}, Assignments: []domain.StoryAssignment{{SessionID: manager.ID, Role: "manager"}}}); err != nil {
		t.Fatal(err)
	}
	// The assignment turn is queued while the session still reads idle.
	if _, err = db.AppendUserMessage(ctx, "phase-thread", manager.ID, "phase-generation", domain.ConversationMessage{ID: "assignment", ClientMessageID: "assignment", Text: "TEST assignment", Origin: domain.MessageOriginAutomation}, "turn-assignment", now); err != nil {
		t.Fatal(err)
	}
	sender := &phaseTurnSender{db: db, sent: make(chan ports.ChatUserMessage, 4)}
	events := cdc.NewBroadcaster()
	poller := cdc.NewPoller(db, events, cdc.PollerConfig{Interval: 10 * time.Millisecond})
	if err = poller.SeekToHead(ctx); err != nil {
		t.Fatal(err)
	}
	pollDone := poller.Start(ctx)
	recovered := make(chan struct{})
	close(recovered)
	runner := &storysvc.Continuations{Store: db, Stories: &storysvc.Service{Store: db}, Sender: sender}
	done := startStoryContinuations(ctx, recovered, events, runner, slog.New(slog.DiscardHandler))
	defer func() { cancel(); <-done; <-pollDone }()

	select {
	case msg := <-sender.sent:
		t.Fatal("continuation queued behind the manager's own open turn", msg.ClientMessageID)
	case <-time.After(500 * time.Millisecond):
	}
	if err = db.SettleTurnByID(ctx, "turn-assignment", domain.TurnStateCompleted, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	// Provider activity updates wake CDC in production; make that wake explicit.
	if err = db.UpdateSession(ctx, manager); err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-sender.sent:
		if !strings.Contains(msg.Text, "Story story;") {
			t.Fatal("wrong story", msg.Text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("continuation was not retained after the manager's turn settled")
	}
}

type reportSubmissions struct{ targets []domain.SessionID }

func (r *reportSubmissions) Submit(_ context.Context, target domain.SessionID, _, _ string) error {
	r.targets = append(r.targets, target)
	return nil
}
func (r *reportSubmissions) Interrupt(context.Context, domain.SessionID) error { return nil }

// With the real store, a worker's settled Done for an already-Done autonomous
// story is acknowledged without a manager turn; an open story's Done still wakes.
func TestOutboxDoneForAlreadyDoneStoryDoesNotWakeManager(t *testing.T) {
	db, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	if err = db.UpsertProject(ctx, domain.ProjectRecord{ID: "repo", Path: t.TempDir(), RegisteredAt: now}); err != nil {
		t.Fatal(err)
	}
	manager, err := db.CreateSession(ctx, domain.SessionRecord{ProjectID: "repo", Kind: domain.KindOrchestrator, Harness: domain.HarnessCodex, Mode: domain.SessionModeChat, CreatedAt: now, UpdatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := db.CreateSession(ctx, domain.SessionRecord{ProjectID: "repo", Kind: domain.KindWorker, Harness: domain.HarnessCodex, CreatedAt: now, UpdatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.CreateEpic(ctx, domain.Epic{ID: "epic", Title: "TEST outbox"}); err != nil {
		t.Fatal(err)
	}
	for id, status := range map[string]string{"finished": "done", "open": "in_review"} {
		story := domain.Story{ID: id, EpicID: "epic", Title: "TEST " + id, Autonomous: true, Status: status, Revision: 1, ProjectIDs: []domain.ProjectID{"repo"}, Assignments: []domain.StoryAssignment{{SessionID: manager.ID, Role: "manager"}, {SessionID: worker.ID, Role: "worker"}}}
		if err = db.CreateStory(ctx, story); err != nil {
			t.Fatal(err)
		}
	}
	reports := reportsvc.New(reportsvc.Deps{Store: db, Now: func() time.Time { return now }})
	for _, id := range []string{"finished", "open"} {
		if _, err = reports.Create(ctx, reportsvc.CreateInput{SessionID: worker.ID, StoryID: id, ClientReportID: "done-" + id, State: domain.ReportDone, Note: "worker finished " + id}); err != nil {
			t.Fatal(err)
		}
	}
	delivery := &reportSubmissions{}
	later := now.Add(domain.ReportSettlementWindow + time.Second)
	coordinator := reportsvc.NewCoordinator(reportsvc.CoordinatorDeps{Store: db, Delivery: delivery, Now: func() time.Time { return later }})
	if _, err = coordinator.RunDue(ctx); err != nil {
		t.Fatal(err)
	}
	if len(delivery.targets) != 1 || delivery.targets[0] != manager.ID {
		t.Fatalf("manager submissions = %v, want exactly one for the open story", delivery.targets)
	}
	pending, err := db.ListPendingReportSchedule(ctx, -1)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("redundant report left pending: %+v", pending)
	}
}
