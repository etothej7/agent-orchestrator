package chat_test

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
)

func TestStoryIntakeHTTPAtomicScopeSourceAndRetry(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	for _, id := range []string{"api", "ui"} {
		if err := h.st.UpsertProject(ctx, domain.ProjectRecord{ID: id, Path: t.TempDir(), RegisteredAt: h.now()}); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.st.SetCoordination(ctx, domain.CoordinationConfig{CoordinatorProjectID: testProject, ManagedProjectIDs: []domain.ProjectID{"api", "ui"}}, h.now()); err != nil {
		t.Fatal(err)
	}
	for _, source := range []struct {
		id     string
		origin domain.MessageOrigin
	}{{"ed-request", domain.MessageOriginHuman}, {"agent-report", domain.MessageOriginAutomation}} {
		_, err := h.st.AppendUserMessage(ctx, h.ctrl.ConversationID(), testSession, h.ctrl.Generation(), domain.ConversationMessage{ID: source.id, Origin: source.origin, Text: "Create the API and UI greeting", ClientMessageID: source.id}, source.id+"-turn", h.now())
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := h.st.AdoptProviderTurn(ctx, h.ctrl.ConversationID(), testSession, h.ctrl.Generation(), "imported-turn", "imported-provider", h.now()); err != nil {
		t.Fatal(err)
	}
	if err := h.st.AppendImportedUserMessage(ctx, h.ctrl.ConversationID(), "imported-provider", domain.ConversationMessage{ID: "provider-import", Text: "Unverified recovered prompt", ClientMessageID: "provider-client"}, h.now()); err != nil {
		t.Fatal(err)
	}

	handler := httpd.NewRouterWithControl(config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, httpd.APIDeps{Stories: &storysvc.Service{Store: h.st}}, httpd.ControlDeps{})
	plan := `{"id":"intake-1","sourceMessageId":"ed-request","sessionId":"p1-1","epic":{"id":"e","title":"TEST greeting"},"stories":[{"id":"api-s","title":"API greeting","scope":"Return a greeting; tests required","projectIds":["api"]},{"id":"ui-s","title":"Shared greeting","scope":"Render the greeting; tests required","projectIds":["api","ui"]}]}`
	post := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/workspace/intake", strings.NewReader(body)))
		return w
	}
	assertEmpty := func() {
		t.Helper()
		epics, err := h.st.ListEpics(ctx)
		if err != nil || len(epics) != 0 {
			t.Fatal("partial epic escaped transaction", epics, err)
		}
		stories, err := h.st.ListStories(ctx)
		if err != nil || len(stories) != 0 {
			t.Fatal("partial stories escaped transaction", stories, err)
		}
	}
	for _, bad := range []string{strings.ReplaceAll(plan, `"api","ui"`, `"api","unmanaged"`), strings.ReplaceAll(plan, "ed-request", "agent-report"), strings.ReplaceAll(plan, "ed-request", "missing-human"), strings.ReplaceAll(plan, "ed-request", "provider-import")} {
		w := post(bad)
		if w.Code != 400 {
			t.Fatal(w.Code, w.Body.String())
		}
		assertEmpty()
	}
	for range 2 {
		w := post(plan)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	stories, err := h.st.ListStories(ctx)
	if err != nil || len(stories) != 2 {
		t.Fatal(stories, err)
	}
	for _, story := range stories {
		if !story.Autonomous || story.Status != "backlog" || len(story.Assignments) != 0 {
			t.Fatal("intake fabricated work", story)
		}
	}
	for _, bad := range []string{strings.ReplaceAll(plan, "intake-1", "duplicate-intake"), strings.ReplaceAll(plan, "Return a greeting; tests required", "Different scope")} {
		w := post(bad)
		if w.Code != 409 {
			t.Fatal("duplicate or changed request accepted", w.Code, w.Body.String())
		}
	}
	// Force a later story insert failure: the new epic and earlier story must roll back.
	_, err = h.st.AppendUserMessage(ctx, h.ctrl.ConversationID(), testSession, h.ctrl.Generation(), domain.ConversationMessage{ID: "second-request", Origin: domain.MessageOriginHuman, Text: "Second outcome", ClientMessageID: "second-request"}, "second-turn", h.now())
	if err != nil {
		t.Fatal(err)
	}
	second := strings.NewReplacer("intake-1", "intake-2", "ed-request", "second-request", `"id":"e"`, `"id":"second-epic"`, `"id":"api-s"`, `"id":"new-api"`).Replace(plan)
	w := post(second)
	if w.Code == 200 {
		t.Fatal("duplicate story overwrite accepted")
	}
	epics, _ := h.st.ListEpics(ctx)
	stories, _ = h.st.ListStories(ctx)
	if len(epics) != 1 || len(stories) != 2 {
		t.Fatal("partial failed plan survived", epics, stories)
	}
}
