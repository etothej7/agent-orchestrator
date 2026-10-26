package chat_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
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
	// Output of the turn Ed started is his answer; output of turns started by
	// reports or forwarded agent text is internal.
	if err := h.st.AppendAssistantDelta(ctx, conv, "item", "human-provider", "REPLY_TO_ED", "ack", h.now()); err != nil {
		t.Fatal(err)
	}
	if err := h.st.AppendAssistantDelta(ctx, conv, "report-item", "report-provider", "PRIVATE_REPORT_REPLY", "report-reply", h.now()); err != nil {
		t.Fatal(err)
	}
	if err := h.st.AppendAssistantDelta(ctx, conv, "forwarded-item", "forwarded-provider", "PRIVATE_FORWARDED_REPLY", "forwarded-reply", h.now()); err != nil {
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
	if !strings.Contains(first, "Build the agreed harmless feature") || !strings.Contains(first, "actual-permission") || !strings.Contains(first, "REPLY_TO_ED") || !strings.Contains(first, `"purpose":"human_reply"`) {
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
	if !strings.Contains(string(raw), "PRIVATE_REPORT_REPLY") || !strings.Contains(string(raw), "PRIVATE_FORWARDED_REPLY") {
		t.Fatal("internal history was mutated")
	}
	// The purpose gate applies to every new load, not just a client-side hide.
	_ = read()
}

func TestCoordinatorHumanHTTPPagesByVisibleItems(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := h.st.SetCoordination(ctx, domain.CoordinationConfig{CoordinatorProjectID: testProject}, h.now()); err != nil {
		t.Fatal(err)
	}
	conv := h.ctrl.ConversationID()
	for i := 1; i <= 3; i++ {
		id := fmt.Sprintf("human-%d", i)
		if _, err := h.st.AppendUserMessage(ctx, conv, testSession, h.ctrl.Generation(), domain.ConversationMessage{ID: id, Text: id, Origin: domain.MessageOriginHuman, ClientMessageID: id}, id+"-turn", h.now()); err != nil {
			t.Fatal(err)
		}
		if err := h.st.BindTurnToProvider(ctx, id+"-turn", id+"-provider", h.now()); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.st.UpsertActivity(ctx, conv, "human-3-provider", domain.ConversationActivity{ID: "permission", Kind: domain.ActivityKindApproval, Status: domain.ActivityStatusPending, Summary: "Permission needed", RequestID: "actual-permission", Detail: []byte(`{"command":"echo permitted","raw":"PRIVATE_PROVIDER"}`)}, h.now()); err != nil {
		t.Fatal(err)
	}
	for i := range 40 {
		id := fmt.Sprintf("internal-%d", i)
		if _, err := h.st.AppendUserMessage(ctx, conv, testSession, h.ctrl.Generation(), domain.ConversationMessage{ID: id, Text: "PRIVATE_INTERNAL_PROMPT", Origin: domain.MessageOriginAutomation, ClientMessageID: id}, id+"-turn", h.now()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.svc.RecordHumanDecision(ctx, testSession, chatsvc.HumanDecision{ID: "scope", Purpose: "scope_question", Question: "Which label?", WhyBlocked: "The requested label is unspecified."}); err != nil {
		t.Fatal(err)
	}
	router := httpd.NewRouterWithControl(config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, httpd.APIDeps{Conversations: h.svc}, httpd.ControlDeps{})
	type page struct {
		OldestSequence int64 `json:"oldestSequence"`
		HasMoreBefore  bool  `json:"hasMoreBefore"`
		Messages       []struct {
			ID   string `json:"id"`
			Text string `json:"text"`
		} `json:"messages"`
		Activities []struct {
			RequestID string `json:"requestId"`
		} `json:"activities"`
	}
	read := func(before int64) page {
		t.Helper()
		w := httptest.NewRecorder()
		url := fmt.Sprintf("/api/v1/sessions/%s/conversation?limit=2", testSession)
		if before > 0 {
			url += fmt.Sprintf("&beforeSequence=%d", before)
		}
		router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, url, nil))
		if w.Code != 200 || strings.Contains(w.Body.String(), "PRIVATE_") {
			t.Fatal(w.Code, w.Body.String())
		}
		var p page
		if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	first := read(0)
	if len(first.Messages) != 1 || first.Messages[0].ID != "decision-scope" || len(first.Activities) != 1 || first.Activities[0].RequestID != "actual-permission" || !first.HasMoreBefore {
		t.Fatal(first)
	}
	second := read(first.OldestSequence)
	if len(second.Messages) != 2 || second.Messages[0].ID != "human-2" || second.Messages[1].ID != "human-3" || !second.HasMoreBefore {
		t.Fatal(second)
	}
	third := read(second.OldestSequence)
	if len(third.Messages) != 1 || third.Messages[0].ID != "human-1" || third.HasMoreBefore {
		t.Fatal(third)
	}
}

func TestCoordinatorStartupFailureHTTPIsActionablePrivateAndIdempotent(t *testing.T) {
	startupFailureHTTP(t)
}

func startupFailureHTTP(t *testing.T) string {
	t.Helper()
	h := newHarness(t)
	ctx := context.Background()
	if err := h.st.SetCoordination(ctx, domain.CoordinationConfig{CoordinatorProjectID: testProject}, h.now()); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.Stop(ctx, testSession); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.SetSessionProvisionState(ctx, testSession, domain.SessionProvisionProvisioning, "", h.now()); err != nil {
		t.Fatal(err)
	}
	turn, err := h.svc.QueueUserMessage(ctx, testSession, ports.ChatUserMessage{Text: "Original queued outcome", Origin: domain.MessageOriginHuman, ClientMessageID: "original-startup"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.SetSessionProvisionState(ctx, testSession, domain.SessionProvisionFailed, "PRIVATE_PROVIDER_STARTUP_DETAIL", h.now()); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := h.svc.RecordStartupFailure(ctx, testSession); err != nil {
			t.Fatal(err)
		}
	}
	router := httpd.NewRouterWithControl(config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, httpd.APIDeps{Conversations: h.svc}, httpd.ControlDeps{})
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+string(testSession)+"/conversation", nil))
	body := w.Body.String()
	if w.Code != 200 || strings.Count(body, "Automatic startup attempts have stopped") != 1 || strings.Contains(body, "PRIVATE_") || !strings.Contains(body, "Original queued outcome") || !strings.Contains(body, `"senderDisplayName":"AO"`) {
		t.Fatal(w.Code, body)
	}
	actual, err := h.st.TurnByID(ctx, turn.ID)
	if err != nil || actual.State != domain.TurnStateQueued || actual.ProviderTurnID != "" {
		t.Fatal(actual, err)
	}
	if len(h.conv.sentTexts()) != 0 {
		t.Fatal("startup notice dispatched a prompt")
	}
	return body
}

func TestCoordinatorStartupFailureHTTPAcceptanceClassifier(t *testing.T) {
	path := os.Getenv("AUTONOMY_CLASSIFIER_PATH")
	if path == "" {
		t.Skip("set AUTONOMY_CLASSIFIER_PATH to the reviewed runtime harness acceptance.py")
	}
	body := startupFailureHTTP(t)
	cmd := exec.Command("python3", "-B", "-c", `import importlib.util,json,sys
spec=importlib.util.spec_from_file_location("acceptance",sys.argv[1])
a=importlib.util.module_from_spec(spec)
sys.modules[spec.name]=a
spec.loader.exec_module(a)
body=json.load(sys.stdin)
messages=body["messages"]
notices=[m for m in messages if m.get("purpose")=="blocking_decision"]
assert len(notices)==1, messages
assert a.classify_message(notices[0])=="blocking_decision", notices
bad=dict(notices[0],text="Startup failed. Why your input is needed: ")
assert a.classify_message(bad)=="non_actionable_or_unclassified"
`, path)
	cmd.Stdin = strings.NewReader(body)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("actual acceptance classifier: %v\n%s", err, out)
	}
}

// Visibility follows what started the turn, whatever prompt the coordinator was
// launched with: Ed's replies show, automation's chatter does not, and an
// explicit question to Ed shows wherever it was asked.
func TestCoordinatorReplyVisibilityIsTurnScoped(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := h.st.SetCoordination(ctx, domain.CoordinationConfig{CoordinatorProjectID: testProject}, h.now()); err != nil {
		t.Fatal(err)
	}
	conv := h.ctrl.ConversationID()
	turns := []struct {
		id, clientID string
		origin       domain.MessageOrigin
		sender, text string
		reply        string
	}{
		{"ask", "ask-1", domain.MessageOriginHuman, "", "Ed asks how the story is going", "REPLY_ANSWERING_ED"},
		{"empty-id", "", domain.MessageOriginHuman, "", "Ed typed this without a client id", "REPLY_TO_EMPTY_ID"},
		{"batch", "report-batch:rpt_1", domain.MessageOriginAutomation, "", "Reports since your previous turn: routine", "PRIVATE_BATCH_CHATTER"},
		{"continue", "story-continue-abc-1-1", domain.MessageOriginAutomation, "", "AO internal story continuation", "PRIVATE_CONTINUE_CHATTER"},
		{"worker", "worker-send", domain.MessageOriginHuman, "worker-1", "A worker message", "PRIVATE_WORKER_CHATTER"},
	}
	for _, turn := range turns {
		_, err := h.st.AppendUserMessage(ctx, conv, testSession, h.ctrl.Generation(), domain.ConversationMessage{ID: turn.id, Origin: turn.origin, Text: turn.text, SenderSessionID: turn.sender, ClientMessageID: turn.clientID}, turn.id+"-turn", h.now())
		if err != nil {
			t.Fatal(err)
		}
		if err = h.st.BindTurnToProvider(ctx, turn.id+"-turn", turn.id+"-provider", h.now()); err != nil {
			t.Fatal(err)
		}
		if err = h.st.AppendAssistantDelta(ctx, conv, turn.id+"-item", turn.id+"-provider", turn.reply, turn.id+"-reply", h.now()); err != nil {
			t.Fatal(err)
		}
		if err = h.st.UpsertActivity(ctx, conv, turn.id+"-provider", domain.ConversationActivity{ID: turn.id + "-reasoning", Kind: domain.ActivityKindReasoning, Status: domain.ActivityStatusCompleted, Summary: "PRIVATE_REASONING_" + turn.id}, h.now()); err != nil {
			t.Fatal(err)
		}
	}
	// A question to Ed from inside an automation-started turn stays visible.
	if _, err := h.svc.RecordHumanDecision(ctx, testSession, chatsvc.HumanDecision{ID: "scope", Purpose: "scope_question", Question: "EXPLICIT_QUESTION_TO_ED", WhyBlocked: "scope unclear"}); err != nil {
		t.Fatal(err)
	}
	handler := httpd.NewRouterWithControl(config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, httpd.APIDeps{Conversations: h.svc}, httpd.ControlDeps{})
	for _, query := range []string{"", "?limit=3"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+string(testSession)+"/conversation"+query, nil))
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		body := w.Body.String()
		if strings.Contains(body, "PRIVATE_") {
			t.Fatal("automation-started output or reasoning leaked", query, body)
		}
		for _, want := range []string{"EXPLICIT_QUESTION_TO_ED", "Ed typed this without a client id", "REPLY_TO_EMPTY_ID"} {
			if !strings.Contains(body, want) {
				t.Fatal("missing visible item", want, query, body)
			}
		}
		if query == "" {
			for _, want := range []string{"REPLY_ANSWERING_ED", "Ed asks how the story is going"} {
				if !strings.Contains(body, want) {
					t.Fatal("missing visible item", want, body)
				}
			}
		}
	}
	// Nothing is lost: the explicit internal view opens every stored item.
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+string(testSession)+"/conversation?audience=all", nil))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, want := range []string{"PRIVATE_BATCH_CHATTER", "PRIVATE_CONTINUE_CHATTER", "PRIVATE_WORKER_CHATTER", "PRIVATE_REASONING_batch", "REPLY_ANSWERING_ED", "EXPLICIT_QUESTION_TO_ED"} {
		if !strings.Contains(w.Body.String(), want) {
			t.Fatal("internal view lost", want, w.Body.String())
		}
	}
	var tagged struct {
		Turns      []struct{ ID, Audience string }                `json:"turns"`
		Messages   []struct{ ID, Audience, Purpose, Text string } `json:"messages"`
		Activities []struct{ ID, Audience string }                `json:"activities"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &tagged); err != nil {
		t.Fatal(err)
	}
	audience := map[string]string{}
	for _, m := range tagged.Messages {
		if m.Audience != "human" && m.Audience != "internal" {
			t.Fatal("untagged message", m)
		}
		audience[strings.SplitN(m.Text, "\n", 2)[0]] = m.Audience
	}
	for text, want := range map[string]string{"REPLY_ANSWERING_ED": "human", "EXPLICIT_QUESTION_TO_ED": "human", "REPLY_TO_EMPTY_ID": "human", "PRIVATE_BATCH_CHATTER": "internal", "PRIVATE_CONTINUE_CHATTER": "internal", "PRIVATE_WORKER_CHATTER": "internal", "Reports since your previous turn: routine": "internal"} {
		if audience[text] != want {
			t.Fatalf("message %q audience = %q, want %q", text, audience[text], want)
		}
	}
	for _, a := range tagged.Activities {
		if a.Audience != "internal" {
			t.Fatal("reasoning must be tagged internal", a)
		}
	}
	turnAudience := map[string]string{}
	for _, turn := range tagged.Turns {
		turnAudience[turn.ID] = turn.Audience
	}
	if turnAudience["ask-turn"] != "human" || turnAudience["batch-turn"] != "internal" || turnAudience["continue-turn"] != "internal" {
		t.Fatal("turn audiences", turnAudience)
	}
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+string(testSession)+"/conversation?audience=everyone", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatal("unknown audience accepted", w.Code)
	}
}

type recordingHumanNotifier struct{ intents []ports.NotificationIntent }

func (n *recordingHumanNotifier) Notify(_ context.Context, intent ports.NotificationIntent) error {
	n.intents = append(n.intents, intent)
	return nil
}

// The two ways to address Ed both show a message and raise a notification;
// notices never pause work and cannot become a chatter channel.
func TestCoordinatorAddressesHumanWithNotificationAndBoundedNotices(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := h.st.SetCoordination(ctx, domain.CoordinationConfig{CoordinatorProjectID: testProject, ManagedProjectIDs: []domain.ProjectID{"repo"}}, h.now()); err != nil {
		t.Fatal(err)
	}
	if err := h.st.CreateEpic(ctx, domain.Epic{ID: "epic", Title: "TEST notices"}); err != nil {
		t.Fatal(err)
	}
	story := domain.Story{ID: "story", EpicID: "epic", Title: "TEST notices", Autonomous: true, Status: "in_progress", Revision: 1, ProjectIDs: []domain.ProjectID{"repo"}}
	if err := h.st.CreateStory(ctx, story); err != nil {
		t.Fatal(err)
	}
	notifier := &recordingHumanNotifier{}
	svc := chatsvc.New(chatsvc.Options{Store: h.st, Sessions: h.st, Notifier: notifier, Now: h.now})

	// A blocking question pauses its story and notifies once, even when retried.
	ask := chatsvc.HumanDecision{ID: "q", StoryID: "story", Purpose: "scope_question", Question: "Blue or Green?", WhyBlocked: "Unspecified."}
	for range 2 {
		if _, err := svc.RecordHumanDecision(ctx, testSession, ask); err != nil {
			t.Fatal(err)
		}
	}
	paused, err := h.st.GetStory(ctx, "story")
	if err != nil || paused.Question == "" {
		t.Fatal("ask must pause the story", paused, err)
	}
	// Notices never pause or retarget work, and need no blocking reason.
	notice := func(id, purpose string) error {
		_, err := svc.RecordHumanDecision(ctx, testSession, chatsvc.HumanDecision{ID: id, StoryID: "story", Purpose: purpose, Question: "Verified at abc123."})
		return err
	}
	for i, purpose := range []string{"verified_result", "stopped_work", "verified_result", "verified_result"} {
		if err := notice(fmt.Sprintf("n%d", i), purpose); err != nil {
			t.Fatal(err)
		}
	}
	if err := notice("n0", "verified_result"); err != nil {
		t.Fatal("retry of an accepted notice must be a no-op", err)
	}
	err = notice("n4", "verified_result")
	var limited *apierr.Error
	if !errors.As(err, &limited) || limited.Code != "COORDINATOR_NOTICE_RATE_LIMITED" || !strings.Contains(limited.Message, "Retry after") {
		t.Fatal("fourth verified result inside the window was not bounded with a retry hint", err)
	}
	// A stop is never dropped, however many results were just sent.
	for _, id := range []string{"stop-a", "stop-b"} {
		if err := notice(id, "stopped_work"); err != nil {
			t.Fatal("stopped work must never be rate limited", err)
		}
	}
	after, err := h.st.GetStory(ctx, "story")
	if err != nil || after.Question != paused.Question || after.Revision != paused.Revision {
		t.Fatal("a notice changed the story", after, err)
	}
	if len(notifier.intents) != 2+4+2+1 {
		// the ask and its retry, four accepted notices, two more stops, and the retry
		// of n0; retries share a source key, so the store raises each notification once.
		t.Fatal("notifications", len(notifier.intents))
	}
	keys := map[string]int{}
	for _, intent := range notifier.intents {
		if intent.Type != domain.NotificationCoordinatorMessage || intent.SessionID != testSession || intent.Title == "" || intent.Body == "" {
			t.Fatal("unusable notification", intent)
		}
		keys[intent.SourceKey]++
	}
	if len(keys) != 7 || keys["coordinator-message:decision-n0"] != 2 {
		t.Fatal("notification keys must be per message so retries dedupe", keys)
	}
	handler := httpd.NewRouterWithControl(config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, httpd.APIDeps{Conversations: h.svc}, httpd.ControlDeps{})
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+string(testSession)+"/conversation", nil))
	if body := w.Body.String(); !strings.Contains(body, "Blue or Green?") || !strings.Contains(body, `"purpose":"verified_result"`) || !strings.Contains(body, `"purpose":"stopped_work"`) {
		t.Fatal("notices must be visible to Ed", body)
	}
}

// An existing coordinator learns the current rules in place: one durable
// internal message per rule revision, never resent, never shown to Ed.
func TestCoordinatorStandingInstructionsDeliverOncePerRevisionAndStayInternal(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := h.st.SetCoordination(ctx, domain.CoordinationConfig{CoordinatorProjectID: testProject}, h.now()); err != nil {
		t.Fatal(err)
	}
	sends := len(h.conv.sentTexts())
	for range 3 {
		delivered, err := h.svc.DeliverCoordinatorStandingInstructions(ctx)
		if err != nil || !delivered {
			t.Fatal(delivered, err)
		}
	}
	texts := h.conv.sentTexts()[sends:]
	if len(texts) != 1 || !strings.Contains(texts[0], "ao tell --id") || !strings.Contains(texts[0], domain.CoordinatorStandingRevision()) || !strings.Contains(texts[0], "Do not reply to Ed about it") {
		t.Fatal("standing instruction not delivered exactly once", texts)
	}
	conv := h.ctrl.ConversationID()
	if _, found, err := h.st.ConversationMessageByClientID(ctx, conv, chatsvc.CoordinatorStandingMessageID(domain.CoordinatorStandingRevision())); err != nil || !found {
		t.Fatal("delivery is not durable", found, err)
	}
	// The coordinator's answer to the update is internal housekeeping.
	if err := h.st.AppendAssistantDelta(ctx, conv, "standing-item", "standing-provider", "PRIVATE_STANDING_ACK", "standing-ack", h.now()); err != nil {
		t.Fatal(err)
	}
	handler := httpd.NewRouterWithControl(config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, httpd.APIDeps{Conversations: h.svc}, httpd.ControlDeps{})
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+string(testSession)+"/conversation", nil))
	if body := w.Body.String(); strings.Contains(body, "PRIVATE_") || strings.Contains(body, "AO standing instruction update") {
		t.Fatal("standing instruction surfaced to Ed", body)
	}
}

// Ed typing into a running automation turn (a steer, or a queued message
// promoted into it) is recorded as steer guidance, not a message. His words and
// everything the coordinator then says in that turn are shown; what automation
// said before stays internal, and an agent relaying a steer never opens a turn.
func TestCoordinatorHumanSteerIntoAutomationTurnIsVisibleAfterwards(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := h.st.SetCoordination(ctx, domain.CoordinationConfig{CoordinatorProjectID: testProject}, h.now()); err != nil {
		t.Fatal(err)
	}
	conv := h.ctrl.ConversationID()
	for _, turn := range []struct{ id, clientID string }{{"steered", "report-batch:steered"}, {"relayed", "report-batch:relayed"}} {
		if _, err := h.st.AppendUserMessage(ctx, conv, testSession, h.ctrl.Generation(), domain.ConversationMessage{ID: turn.id, Origin: domain.MessageOriginAutomation, Text: "Reports since your previous turn", ClientMessageID: turn.clientID}, turn.id+"-turn", h.now()); err != nil {
			t.Fatal(err)
		}
		if err := h.st.BindTurnToProvider(ctx, turn.id+"-turn", turn.id+"-provider", h.now()); err != nil {
			t.Fatal(err)
		}
		if err := h.st.AppendAssistantDelta(ctx, conv, turn.id+"-before", turn.id+"-provider", "PRIVATE_BEFORE_"+turn.id, turn.id+"-before-msg", h.now()); err != nil {
			t.Fatal(err)
		}
	}
	steer := func(turn, id, extra string) {
		t.Helper()
		detail := `{"event":"steer","text":"STEER_TEXT_` + id + `","origin":"human","clientMessageId":"` + id + `"` + extra + `}`
		if err := h.st.UpsertActivity(ctx, conv, turn+"-provider", domain.ConversationActivity{ID: "steer-" + id, Kind: domain.ActivityKindSystem, Status: domain.ActivityStatusCompleted, Summary: "Steered", Detail: []byte(detail), ProviderItemID: "ao-steer:" + id}, h.now()); err != nil {
			t.Fatal(err)
		}
		if err := h.st.AppendAssistantDelta(ctx, conv, turn+"-after", turn+"-provider", "AFTER_"+id, turn+"-after-msg", h.now()); err != nil {
			t.Fatal(err)
		}
	}
	steer("steered", "ed", "")
	steer("relayed", "agent", `,"senderSessionId":"worker-1"`)

	handler := httpd.NewRouterWithControl(config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, httpd.APIDeps{Conversations: h.svc}, httpd.ControlDeps{})
	for _, query := range []string{"", "?limit=4"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+string(testSession)+"/conversation"+query, nil))
		body := w.Body.String()
		if w.Code != 200 || strings.Contains(body, "PRIVATE_BEFORE_") || strings.Contains(body, "AFTER_agent") || strings.Contains(body, "STEER_TEXT_agent") {
			t.Fatal("automation output or an agent-relayed steer leaked", query, w.Code, body)
		}
		for _, want := range []string{"STEER_TEXT_ed", "AFTER_ed"} {
			if !strings.Contains(body, want) {
				t.Fatal("Ed's steer or the reply to it is hidden", want, query, body)
			}
		}
		if strings.Contains(body, "ao-steer:") {
			t.Fatal("provider item key leaked", body)
		}
	}
}
