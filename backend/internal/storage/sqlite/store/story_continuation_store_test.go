package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	storysvc "github.com/aoagents/agent-orchestrator/backend/internal/service/story"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

type continuationStore struct {
	*sqlite.Store
	turn domain.ConversationTurn
}

func (s continuationStore) TurnByID(context.Context, string) (domain.ConversationTurn, error) {
	return s.turn, nil
}

type continuationSender struct {
	mu       sync.Mutex
	messages []ports.ChatUserMessage
	err      error
	entered  chan struct{}
	release  chan struct{}
}

func (s *continuationSender) Send(_ context.Context, _ domain.SessionID, msg ports.ChatUserMessage) (domain.ConversationTurn, error) {
	s.mu.Lock()
	s.messages = append(s.messages, msg)
	s.mu.Unlock()
	if s.entered != nil {
		s.entered <- struct{}{}
		<-s.release
	}
	return domain.ConversationTurn{ID: "actual-turn", State: domain.TurnStateCompleted}, s.err
}

func setupContinuation(t *testing.T) (*sqlite.Store, string, domain.SessionID) {
	t.Helper()
	dir := t.TempDir()
	db, err := sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	seedProject(t, db, "repo")
	seedProject(t, db, "coordinator")
	if err := db.SetCoordination(context.Background(), domain.CoordinationConfig{CoordinatorProjectID: "coordinator", ManagedProjectIDs: []domain.ProjectID{"repo"}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	rec := sampleRecord("repo")
	rec.Kind = domain.KindOrchestrator
	rec.Activity.State = domain.ActivityIdle
	manager, err := db.CreateSession(context.Background(), rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateEpic(context.Background(), domain.Epic{ID: "epic", Title: "TEST"}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateStory(context.Background(), domain.Story{ID: "story", EpicID: "epic", Title: "TEST", Autonomous: true, Status: "backlog", Revision: 1, ProjectIDs: []domain.ProjectID{"repo"}, Assignments: []domain.StoryAssignment{{SessionID: manager.ID, Role: "manager"}}}); err != nil {
		t.Fatal(err)
	}
	return db, dir, manager.ID
}

func TestStoryContinuationUncertainDeliveryRetainsHandleAcrossReopen(t *testing.T) {
	db, dir, _ := setupContinuation(t)
	ctx := context.Background()
	now := time.Now().UTC()
	sender := &continuationSender{err: errors.New("transport lost after accepting")}
	run := func(db *sqlite.Store) {
		t.Helper()
		st := continuationStore{Store: db}
		c := storysvc.Continuations{Store: st, Stories: &storysvc.Service{Store: st}, Sender: sender, Now: func() time.Time { return now }}
		if err := c.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	run(db)
	first, ok, err := db.GetStoryContinuation(ctx, "story")
	if err != nil || !ok {
		t.Fatal(first, ok, err)
	}
	if first.State != "sending" || first.DeliveryAttempts != 1 {
		t.Fatal(first)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now = now.Add(2 * time.Minute)
	run(db)
	now = now.Add(2 * time.Minute)
	run(db)
	now = now.Add(2 * time.Minute)
	run(db)
	final, _, err := db.GetStoryContinuation(ctx, "story")
	if err != nil {
		t.Fatal(err)
	}
	if len(sender.messages) != 3 || final.State != "blocked" {
		t.Fatalf("messages=%d ledger=%+v", len(sender.messages), final)
	}
	for _, msg := range sender.messages {
		if msg.ClientMessageID != first.DeliveryID || msg.Text != first.Text {
			t.Fatal("uncertainty changed delivery identity")
		}
	}
	story, err := db.GetStory(ctx, "story")
	if err != nil || story.Status != "backlog" {
		t.Fatal("delivery inferred completion", story, err)
	}
}

func TestStoryContinuationConcurrentLeasePreventsDoubleDispatch(t *testing.T) {
	db, _, _ := setupContinuation(t)
	defer db.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	sender := &continuationSender{entered: make(chan struct{}, 2), release: make(chan struct{})}
	st := continuationStore{Store: db}
	c := storysvc.Continuations{Store: st, Stories: &storysvc.Service{Store: st}, Sender: sender, Now: func() time.Time { return now }}
	done := make(chan error, 1)
	go func() { done <- c.RunOnce(ctx) }()
	<-sender.entered
	if err := c.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	close(sender.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(sender.messages) != 1 {
		t.Fatalf("dispatched %d times", len(sender.messages))
	}
}

func TestStoryContinuationCompletedTurnsWithoutProgressAreBounded(t *testing.T) {
	db, _, _ := setupContinuation(t)
	defer db.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	st := continuationStore{Store: db, turn: domain.ConversationTurn{ID: "actual-turn", State: domain.TurnStateCompleted}}
	sender := &continuationSender{}
	c := storysvc.Continuations{Store: st, Stories: &storysvc.Service{Store: st}, Sender: sender, Now: func() time.Time { return now }}
	for range 5 {
		if err := c.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		now = now.Add(2 * time.Minute)
	}
	rec, _, _ := db.GetStoryContinuation(ctx, "story")
	if len(sender.messages) != 3 || rec.State != "blocked" {
		t.Fatal(len(sender.messages), rec)
	}
	if sender.messages[0].ClientMessageID == sender.messages[1].ClientMessageID {
		t.Fatal("new known-completed attempt needs distinct identity")
	}
}

func TestStoryContinuationIdleDoneAndAwaitingHumanDoNotMeanStalled(t *testing.T) {
	for _, kind := range []string{"done", "question"} {
		t.Run(kind, func(t *testing.T) {
			db, _, manager := setupContinuation(t)
			defer db.Close()
			ctx := context.Background()
			s, _ := db.GetStory(ctx, "story")
			if kind == "done" {
				s.Status = "done"
			} else {
				s.Question = "Which required behavior?"
				s.QuestionRequesterSessionID = manager
				s.QuestionEventID = "q"
			}
			if _, _, err := db.ApplyStoryEvent(ctx, s, domain.StoryEvent{ID: "fact", StoryID: s.ID, Kind: kind}, `{}`); err != nil {
				t.Fatal(err)
			}
			st := continuationStore{Store: db}
			sender := &continuationSender{}
			c := storysvc.Continuations{Store: st, Stories: &storysvc.Service{Store: st}, Sender: sender}
			if err := c.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			rec, _, _ := db.GetStoryContinuation(ctx, "story")
			want := "completed"
			if kind == "question" {
				want = "waiting_for_ed"
			}
			if len(sender.messages) != 0 || rec.State != want {
				t.Fatal(rec)
			}
		})
	}
}

func TestStoryContinuationQuestionPausesUncertainHandleAndResumeIsIdempotent(t *testing.T) {
	db, _, manager := setupContinuation(t)
	defer db.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	sender := &continuationSender{err: errors.New("lost response")}
	st := continuationStore{Store: db}
	c := storysvc.Continuations{Store: st, Stories: &storysvc.Service{Store: st}, Sender: sender, Now: func() time.Time { return now }}
	if err := c.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	original, _, _ := db.GetStoryContinuation(ctx, "story")
	s, _ := db.GetStory(ctx, "story")
	s.Question = "Which behavior?"
	s.QuestionRequesterSessionID = manager
	s.QuestionEventID = "question"
	if _, _, err := db.ApplyStoryEvent(ctx, s, domain.StoryEvent{ID: "question", StoryID: s.ID, Kind: "question"}, `{}`); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	if err := c.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	paused, _, _ := db.GetStoryContinuation(ctx, "story")
	if paused.DeliveryID != original.DeliveryID || paused.State != "sending" || len(sender.messages) != 1 {
		t.Fatal(paused)
	}
	status, err := c.Status(ctx, "story")
	if err != nil || status.State != "waiting_for_ed" {
		t.Fatal(status, err)
	}
	// Simulate an independently delivered answer fact; do not replace its receipt.
	s, _ = db.GetStory(ctx, "story")
	s.Question = ""
	s.QuestionRequesterSessionID = ""
	s.QuestionEventID = ""
	if _, _, err := db.ApplyStoryEvent(ctx, s, domain.StoryEvent{ID: "answered", StoryID: s.ID, Kind: "answer"}, `{}`); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := c.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Minute * 2)
	}
	blocked, _, _ := db.GetStoryContinuation(ctx, "story")
	if blocked.State != "blocked" {
		t.Fatal(blocked)
	}
	if _, err := c.Resume(ctx, "story", "recover-original", blocked.Revision-1); err == nil {
		t.Fatal("stale resume accepted")
	}
	if _, err := c.Resume(ctx, "story", "recover-original", blocked.Revision); err != nil {
		t.Fatal(err)
	}
	if err := c.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Resume(ctx, "story", "recover-original", blocked.Revision); err != nil {
		t.Fatal("lost recovery response is not retryable", err)
	}
	if len(sender.messages) != 4 {
		t.Fatal(len(sender.messages))
	}
	for _, m := range sender.messages {
		if m.ClientMessageID != original.DeliveryID || m.Text != original.Text {
			t.Fatal("changed uncertain send identity", m)
		}
	}
}

func TestStoryContinuationActiveAndUnknownTurnsAreNotRedispatched(t *testing.T) {
	for _, state := range []domain.TurnState{domain.TurnStateRunning, domain.TurnStateQueued, "unknown"} {
		t.Run(string(state), func(t *testing.T) {
			db, _, _ := setupContinuation(t)
			defer db.Close()
			ctx := context.Background()
			now := time.Now().UTC()
			st := continuationStore{Store: db, turn: domain.ConversationTurn{ID: "actual-turn", State: state}}
			sender := &continuationSender{}
			c := storysvc.Continuations{Store: st, Stories: &storysvc.Service{Store: st}, Sender: sender, Now: func() time.Time { return now }}
			if err := c.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			now = now.Add(time.Minute * 2)
			if err := c.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			if len(sender.messages) != 1 {
				t.Fatal("interrupted outstanding turn")
			}
			now = now.Add(time.Hour)
			if err := c.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			rec, _, _ := db.GetStoryContinuation(ctx, "story")
			if rec.State != "blocked" || len(sender.messages) != 1 {
				t.Fatal(rec)
			}
		})
	}
}
