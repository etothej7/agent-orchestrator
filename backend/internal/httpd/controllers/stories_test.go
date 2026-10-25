package controllers_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
	storysvc "github.com/aoagents/agent-orchestrator/backend/internal/service/story"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

type privacyStoryStore struct{ storysvc.Store }

func (*privacyStoryStore) StoryReplies(context.Context, string) ([]domain.StoryReplyThread, error) {
	return []domain.StoryReplyThread{}, nil
}

func (*privacyStoryStore) GetStory(context.Context, string) (domain.Story, error) {
	return domain.Story{ID: "s", Title: "Public scope", Status: "backlog"}, nil
}
func (*privacyStoryStore) StoryEvents(context.Context, string) ([]domain.StoryEvent, error) {
	return []domain.StoryEvent{{ID: "report-r", StoryID: "s", Kind: "report", SenderSessionID: "worker", Text: "Public report", Reference: "r", Result: "done"}, {ID: "message", StoryID: "s", Kind: "message", RecipientSessionID: "manager", Text: "Public message", Reference: "turn", DeliveryKind: "turn", Result: "queued"}, {ID: "review-run", StoryID: "s", Kind: "native_review", ReviewID: "real-reviewer", ReviewRunID: "run", Text: "Public review", Reference: "sha", Result: "approved"}}, nil
}
func (*privacyStoryStore) GetReport(context.Context, string) (domain.ReportRecord, bool, error) {
	return domain.ReportRecord{ID: "r", StoryID: "s", SessionID: "worker", ProjectID: "repo", RecipientProjectID: "coordinator", RecipientSessionID: "manager", State: domain.ReportDone, DeliveryState: domain.ReportClaimed, DeliveryBatchID: "batch", DeliveryAttempts: 2, Note: "Public report", ClaimToken: "PRIVATE_CLAIM_SENTINEL", LastError: "PRIVATE_PROVIDER_PAYLOAD_SENTINEL", ClientReportID: "PRIVATE_INTERNAL_KEY_SENTINEL"}, true, nil
}
func (*privacyStoryStore) TurnByID(context.Context, string) (domain.ConversationTurn, error) {
	return domain.ConversationTurn{ID: "turn", State: domain.TurnStateRunning, ProviderTurnID: "PRIVATE_PROVIDER_TURN_SENTINEL", BranchID: "PRIVATE_REASONING_SENTINEL"}, nil
}
func (*privacyStoryStore) FinishStoryDelivery(context.Context, domain.StoryEvent) error { return nil }

func TestStoryHTTPOnlyExposesPublicFactsAndActualDeliveryIdentity(t *testing.T) {
	svc := &storysvc.Service{Store: &privacyStoryStore{}}
	handler := httpd.NewRouterWithControl(config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, httpd.APIDeps{Stories: svc}, httpd.ControlDeps{})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/workspace/stories/s", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, sentinel := range []string{"PRIVATE_CLAIM_SENTINEL", "PRIVATE_PROVIDER_PAYLOAD_SENTINEL", "PRIVATE_INTERNAL_KEY_SENTINEL", "PRIVATE_PROVIDER_TURN_SENTINEL", "PRIVATE_REASONING_SENTINEL"} {
		if strings.Contains(body, sentinel) {
			t.Fatalf("private data leaked: %s", sentinel)
		}
	}
	for _, public := range []string{`"recipientSessionId":"manager"`, `"deliveryState":"claimed"`, `"state":"done"`, `"result":"delivered"`, `"reviewId":"real-reviewer"`, `"reviewRunId":"run"`} {
		if !strings.Contains(body, public) {
			t.Fatalf("missing public fact %s: %s", public, body)
		}
	}
	if strings.Contains(body, `"senderSessionId":"real-reviewer"`) {
		t.Fatal("native reviewer impersonates session")
	}
}

func TestStoryContinuationHTTPAllowlistAndExplicitRecovery(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.CreateEpic(ctx, domain.Epic{ID: "e", Title: "TEST"}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateStory(ctx, domain.Story{ID: "s", EpicID: "e", Title: "TEST", Status: "in_progress", Autonomous: true, Revision: 1}); err != nil {
		t.Fatal(err)
	}
	saved, err := db.SaveStoryContinuation(ctx, domain.StoryContinuation{StoryID: "s", State: "blocked", Checkpoint: "implementation", RecipientSessionID: "manager", Reason: "Choose whether to retry the unavailable provider.", Text: "PRIVATE_INTERNAL_PROMPT", DeliveryID: "PRIVATE_REQUEST_HANDLE", DeliveryAttempts: 3})
	if err != nil || !saved {
		t.Fatal(saved, err)
	}
	handler := httpd.NewRouterWithControl(config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, httpd.APIDeps{Stories: &storysvc.Service{Store: db}}, httpd.ControlDeps{})
	request := func(method, body string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(method, "/api/v1/workspace/stories/s/continuation", strings.NewReader(body)))
		if strings.Contains(rec.Body.String(), "PRIVATE_") {
			t.Fatal("internal delivery facts exposed", rec.Body.String())
		}
		return rec
	}
	rec := request(http.MethodGet, "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"state":"blocked"`) {
		t.Fatal(rec.Code, rec.Body.String())
	}
	for range 2 {
		rec = request(http.MethodPost, `{"id":"human-retry","revision":1}`)
		if rec.Code != 200 {
			t.Fatal(rec.Code, rec.Body.String())
		}
	}
	state, _, err := db.GetStoryContinuation(ctx, "s")
	if err != nil || state.Revision != 2 || state.DeliveryID != "PRIVATE_REQUEST_HANDLE" || state.State != "sending" {
		t.Fatal(state, err)
	}
	rec = request(http.MethodPost, `{"id":"different-retry","revision":1}`)
	if rec.Code != 400 {
		t.Fatal("stale retry accepted", rec.Code)
	}
	story, err := db.GetStory(ctx, "s")
	if err != nil || story.Status != "in_progress" {
		t.Fatal(story, err)
	}
}
