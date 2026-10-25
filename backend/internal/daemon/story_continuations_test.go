package daemon

import (
	"context"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/cdc"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	storysvc "github.com/aoagents/agent-orchestrator/backend/internal/service/story"
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
			select {
			case msg := <-sender.sent:
				if !strings.Contains(msg.Text, "Story "+target+";") {
					t.Fatal("wrong next story", msg.Text)
				}
				t.Logf("%s CDC -> durable next dispatch %s in %s", status, msg.ClientMessageID, time.Since(started))
			case <-time.After(2 * time.Second):
				t.Fatal("phase waited for timer/human instead of CDC")
			}
		})
	}
}
