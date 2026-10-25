package chat_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
)

func TestCoordinatorHumanHTTPHidesStreamingAndBackgroundChatter(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := h.st.SetCoordination(ctx, domain.CoordinationConfig{CoordinatorProjectID: testProject}, h.now()); err != nil {
		t.Fatal(err)
	}
	conv := h.ctrl.ConversationID()
	for _, msg := range []struct {
		id     string
		origin domain.MessageOrigin
		sender string
		text   string
	}{
		{"human", domain.MessageOriginHuman, "", "Build the agreed harmless feature"},
		{"report", domain.MessageOriginAutomation, "worker", "PRIVATE_CHECKPOINT_ACK"},
		{"forwarded", domain.MessageOriginHuman, "manager", "PRIVATE_FORWARDED_REPORT"},
	} {
		_, err := h.st.AppendUserMessage(ctx, conv, testSession, h.ctrl.Generation(), domain.ConversationMessage{ID: msg.id, Origin: msg.origin, Text: msg.text, SenderSessionID: msg.sender, ClientMessageID: msg.id}, msg.id+"-turn", h.now())
		if err != nil {
			t.Fatal(err)
		}
		if err := h.st.BindTurnToProvider(ctx, msg.id+"-turn", msg.id+"-provider", h.now()); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.st.AppendAssistantDelta(ctx, conv, "item", "human-provider", "PRIVATE_RECEIPT_ACK", "ack", h.now()); err != nil {
		t.Fatal(err)
	}
	if err := h.st.UpsertActivity(ctx, conv, "human-provider", domain.ConversationActivity{ID: "reasoning", Kind: domain.ActivityKindReasoning, Status: domain.ActivityStatusRunning, Summary: "PRIVATE_REASONING", Detail: []byte(`{"raw":"PRIVATE_PROVIDER"}`)}, h.now()); err != nil {
		t.Fatal(err)
	}
	if err := h.st.UpsertActivity(ctx, conv, "human-provider", domain.ConversationActivity{ID: "permission", Kind: domain.ActivityKindApproval, Status: domain.ActivityStatusPending, Summary: "Permission needed", RequestID: "actual-permission", Detail: []byte(`{"command":"echo permitted","decisions":[{"id":"allow","label":"Allow"}],"raw":"PRIVATE_APPROVAL_PAYLOAD"}`)}, h.now()); err != nil {
		t.Fatal(err)
	}
	if err := h.st.AdoptProviderTurn(ctx, conv, testSession, h.ctrl.Generation(), "imported-turn", "imported-provider", h.now()); err != nil {
		t.Fatal(err)
	}
	if err := h.st.AppendImportedUserMessage(ctx, conv, "imported-provider", domain.ConversationMessage{ID: "imported", Text: "PRIVATE_IMPORTED_INTERNAL_PROMPT", ClientMessageID: "looks-human"}, h.now()); err != nil {
		t.Fatal(err)
	}

	handler := httpd.NewRouterWithControl(config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, httpd.APIDeps{Conversations: h.svc}, httpd.ControlDeps{})
	read := func() string {
		t.Helper()
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+string(testSession)+"/conversation", nil))
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		body := w.Body.String()
		if strings.Contains(body, "PRIVATE_") {
			t.Fatal("human projection leaked", body)
		}
		return body
	}
	first := read()
	if !strings.Contains(first, "Build the agreed harmless feature") || !strings.Contains(first, "actual-permission") {
		t.Fatal(first)
	}
	decision := chatsvc.HumanDecision{ID: "scope-decision", Purpose: "scope_question", Question: "Should the label be Blue or Green?", WhyBlocked: "The required label is not specified."}
	for range 2 {
		if _, err := h.svc.RecordHumanDecision(ctx, testSession, decision); err != nil {
			t.Fatal(err)
		}
	}
	second := read()
	if strings.Count(second, "Should the label be Blue or Green?") != 1 || !strings.Contains(second, `"purpose":"scope_question"`) {
		t.Fatal(second)
	}
	decision.Question = "Different question"
	if _, err := h.svc.RecordHumanDecision(ctx, testSession, decision); err == nil {
		t.Fatal("reused decision key replaced content")
	}
	snapshot, err := h.svc.Snapshot(ctx, testSession)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(snapshot)
	if !strings.Contains(string(raw), "PRIVATE_RECEIPT_ACK") {
		t.Fatal("internal history was mutated")
	}
	// The purpose gate applies to every new load, not just a client-side hide.
	_ = read()
}
