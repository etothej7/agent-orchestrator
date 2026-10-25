package chat_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	storysvc "github.com/aoagents/agent-orchestrator/backend/internal/service/story"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	sqlstore "github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

func continuationHarness(t *testing.T, overall bool) (*harness, *storysvc.Continuations, http.Handler) {
	t.Helper()
	h := newHarness(t)
	ctx := context.Background()
	other := domain.ProjectID("other")
	if err := h.st.UpsertProject(ctx, domain.ProjectRecord{ID: string(other), Path: t.TempDir(), RegisteredAt: h.now()}); err != nil {
		t.Fatal(err)
	}
	cfg := domain.CoordinationConfig{CoordinatorProjectID: other, ManagedProjectIDs: []domain.ProjectID{testProject}}
	project := testProject
	assignments := []domain.StoryAssignment{{SessionID: testSession, Role: "manager"}}
	if overall {
		cfg = domain.CoordinationConfig{CoordinatorProjectID: testProject, ManagedProjectIDs: []domain.ProjectID{other}}
		project = other
		assignments = nil
	}
	if err := h.st.SetCoordination(ctx, cfg, h.now()); err != nil {
		t.Fatal(err)
	}
	if err := h.st.CreateEpic(ctx, domain.Epic{ID: "e", Title: "TEST"}); err != nil {
		t.Fatal(err)
	}
	if err := h.st.CreateStory(ctx, domain.Story{ID: "s", EpicID: "e", Title: "TEST decision", Autonomous: true, Status: "in_progress", ProjectIDs: []domain.ProjectID{project}, Assignments: assignments}); err != nil {
		t.Fatal(err)
	}
	svc := &storysvc.Service{Store: h.st, Messenger: h.svc}
	c := &storysvc.Continuations{Store: h.st, Stories: svc, Sender: h.svc, Now: h.now}
	router := httpd.NewRouterWithControl(config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, httpd.APIDeps{Stories: svc, Conversations: h.svc}, httpd.ControlDeps{})
	return h, c, router
}
func completeContinuation(t *testing.T, h *harness, state domain.TurnState) domain.StoryContinuation {
	t.Helper()
	rec, _, err := h.st.GetStoryContinuation(context.Background(), "s")
	if err != nil {
		t.Fatal(err)
	}
	turn, err := h.st.TurnByID(context.Background(), rec.TurnID)
	if err != nil {
		t.Fatal(err)
	}
	h.conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: turn.ProviderTurnID, TurnState: state})
	h.awaitSnapshot(t, func(s sqlstore.ConversationSnapshot) bool {
		for _, r := range s.Turns {
			if r.ID == turn.ID && r.State == state {
				return true
			}
		}
		return false
	})
	h.advance(2 * time.Minute)
	return rec
}
func TestContinuationResumeCreatesNewDurableTurnAfterCompletedBudget(t *testing.T) {
	h, c, router := continuationHarness(t, false)
	ctx := context.Background()
	seen := map[string]bool{}
	for range 3 {
		if err := c.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		r := completeContinuation(t, h, domain.TurnStateCompleted)
		if seen[r.TurnID] {
			t.Fatal("replay", r)
		}
		seen[r.TurnID] = true
	}
	if err := c.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	r, _, _ := h.st.GetStoryContinuation(ctx, "s")
	if r.State != "blocked" {
		t.Fatal(r)
	}
	body := fmt.Sprintf(`{"id":"resume-once","revision":%d}`, r.Revision)
	for range 2 {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/workspace/stories/s/continuation", strings.NewReader(body)))
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if err := c.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	r, _, _ = h.st.GetStoryContinuation(ctx, "s")
	if r.TurnID == "" || seen[r.TurnID] || len(h.conv.sentTexts()) != 4 {
		t.Fatal("completed request replayed", r, h.conv.sentTexts())
	}
}
func TestContinuationCheckpointChangeCannotBypassStoppedReceipt(t *testing.T) {
	for _, state := range []domain.TurnState{domain.TurnStateInterrupted, domain.TurnStateCancelled} {
		t.Run(string(state), func(t *testing.T) {
			h, c, _ := continuationHarness(t, false)
			ctx := context.Background()
			if err := c.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			completeContinuation(t, h, state)
			worker, err := h.st.CreateSession(ctx, domain.SessionRecord{ProjectID: testProject, Kind: domain.KindWorker, Harness: domain.HarnessCodex, Mode: domain.SessionModeChat})
			if err != nil {
				t.Fatal(err)
			}
			row, _ := h.st.GetStory(ctx, "s")
			row.Assignments = append(row.Assignments, domain.StoryAssignment{SessionID: worker.ID, Role: "worker"})
			if _, _, err := h.st.ApplyStoryEvent(ctx, row, domain.StoryEvent{ID: "assigned-worker", StoryID: "s", Kind: "assignment", RecipientSessionID: worker.ID}, `{"kind":"assignment"}`); err != nil {
				t.Fatal(err)
			}
			if err := c.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			r, _, _ := h.st.GetStoryContinuation(ctx, "s")
			if r.State != "blocked" || len(h.conv.sentTexts()) != 1 {
				t.Fatal("stop bypassed", r)
			}
		})
	}
}

type pausedContinuationClaim struct {
	*sqlite.Store
	entered, release chan struct{}
}

func (s pausedContinuationClaim) ClaimStoryContinuation(ctx context.Context, r domain.StoryContinuation, rev int64) (bool, error) {
	close(s.entered)
	<-s.release
	return s.Store.ClaimStoryContinuation(ctx, r, rev)
}
func TestContinuationClaimFencesConcurrentQuestionAndRevision(t *testing.T) {
	for _, kind := range []string{"question", "blocked"} {
		t.Run(kind, func(t *testing.T) {
			h, c, _ := continuationHarness(t, false)
			ctx := context.Background()
			gate := pausedContinuationClaim{h.st, make(chan struct{}), make(chan struct{})}
			c.Store = gate
			done := make(chan error, 1)
			go func() { done <- c.RunOnce(ctx) }()
			<-gate.entered
			row, _ := h.st.GetStory(ctx, "s")
			if _, err := c.Stories.Apply(ctx, storysvc.Command{ID: "concurrent", StoryID: "s", Revision: row.Revision, Kind: kind, SenderSessionID: testSession, Text: "Need current decision"}); err != nil {
				t.Fatal(err)
			}
			close(gate.release)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if len(h.conv.sentTexts()) != 0 {
				t.Fatal("stale dispatch")
			}
			_, found, err := h.st.GetStoryContinuation(ctx, "s")
			if err != nil || found {
				t.Fatal("stale lease", found, err)
			}
		})
	}
}
func TestContinuationExhaustionHTTPDoesNotExposeDeliveryHandle(t *testing.T) {
	h, c, router := continuationHarness(t, true)
	ctx := context.Background()
	h.conv.mu.Lock()
	h.conv.sendErr = errors.New("provider unavailable")
	h.conv.mu.Unlock()
	for range 5 {
		if err := c.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		h.advance(2 * time.Minute)
	}
	r, _, err := h.st.GetStoryContinuation(ctx, "s")
	if err != nil || r.DeliveryID == "" || r.State != "blocked" || r.DeliveryAttempts != 3 || r.TurnID != "" {
		t.Fatal(r, err)
	}
	for _, path := range []string{"/api/v1/workspace", "/api/v1/workspace/stories/s", "/api/v1/workspace/stories/s/continuation", "/api/v1/sessions/" + string(testSession) + "/conversation"} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != 200 {
			t.Fatal(path, w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), r.DeliveryID) || strings.Contains(w.Body.String(), r.Text) {
			t.Fatal("internal handle leaked", path, w.Body.String())
		}
	}
}
func TestAskHTTPAtomicallyPausesOwnedStoryUntilDeliveredAnswer(t *testing.T) {
	h, c, router := continuationHarness(t, true)
	ctx := context.Background()
	post := func(path, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
		return w
	}
	base := `{"id":"scope","purpose":"scope_question","question":"Blue or Green?","whyBlocked":"Only Ed knows the label"}`
	path := "/api/v1/sessions/" + string(testSession) + "/conversation/human-decisions"
	if w := post(path, base); w.Code < 400 {
		t.Fatal("unowned question accepted")
	}
	body := strings.Replace(base, `"id":"scope"`, `"id":"scope","storyId":"s"`, 1)
	for range 2 {
		if w := post(path, body); w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	row, _ := h.st.GetStory(ctx, "s")
	if row.QuestionEventID != "decision-scope" || row.QuestionRequesterSessionID != testSession {
		t.Fatal(row)
	}
	for range 4 {
		if err := c.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		h.advance(time.Hour)
	}
	if len(h.conv.sentTexts()) != 0 {
		t.Fatal("continued during pause")
	}
	answer := fmt.Sprintf(`{"id":"ed-answer","storyId":"s","revision":%d,"kind":"answer","questionEventId":"decision-scope","text":"Blue"}`, row.Revision)
	if w := post("/api/v1/workspace/commands", answer); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, err := c.Stories.List(ctx); err != nil {
		t.Fatal(err)
	}
	row, _ = h.st.GetStory(ctx, "s")
	if row.Question != "" || row.Status != "in_progress" || len(h.conv.sentTexts()) != 1 || !strings.Contains(h.conv.sentTexts()[0], "Blue") {
		t.Fatal("answer receipt did not settle pause", row, h.conv.sentTexts())
	}
}
