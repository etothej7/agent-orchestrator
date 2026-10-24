package controllers_test

import (
	"context"
	"encoding/json"
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
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	storysvc "github.com/aoagents/agent-orchestrator/backend/internal/service/story"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

// Matches the original manual trial: an ordinary public assistant response on
// the exact dispatched turn, with no ao report/send command by the recipient.
func TestStoryHTTPProjectsOriginalReplyWithoutTranscriptLeakage(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 10, 24, 20, 25, 31, 0, time.UTC)
	must(db.UpsertProject(ctx, domain.ProjectRecord{ID: "repo", Path: t.TempDir(), RegisteredAt: now}))
	manager, err := db.CreateSession(ctx, domain.SessionRecord{ProjectID: "repo", Kind: domain.KindWorker, Harness: domain.HarnessCodex, Mode: domain.SessionModeChat, CreatedAt: now, UpdatedAt: now})
	must(err)
	conversation, err := db.CreateConversation(ctx, "conversation", domain.ConversationScopeSession, "repo", manager.ID, now)
	must(err)
	must(db.ClaimChatControllerGeneration(ctx, manager.ID, "generation"))
	const exchange = "2d6a481c-1dd9-48db-852e-e82ea50fb74f"
	const turn = "b6cc2933-8a75-45ff-950b-1789fa14daf3"
	const reply = "ede99368-5e6f-44f2-906b-1373d98f4264"
	const original = "Harmless workflow test: please reply “Test reply received” in this story. Do not change files, run commands, or assign work."
	for _, id := range []string{"test-60f4-b1", "other-story"} {
		must(db.CreateEpic(ctx, domain.Epic{ID: id, Title: "TEST"}))
		must(db.CreateStory(ctx, domain.Story{ID: id, EpicID: id, Title: "TEST", Status: "in_progress", Revision: 1}))
	}
	seed := func(story, id, turnID, text string) {
		t.Helper()
		msg := domain.ConversationMessage{ID: "user-" + id, Role: domain.MessageRoleUser, Origin: domain.MessageOriginHuman, Text: text, ClientMessageID: "story-" + id, CreatedAt: now}
		created, err := db.AppendUserMessage(ctx, conversation.ID, manager.ID, "generation", msg, turnID, now)
		must(err)
		if !created {
			t.Fatal("initial message not created")
		}
		created, err = db.AppendUserMessage(ctx, conversation.ID, manager.ID, "generation", msg, "unused-retry-turn", now)
		must(err)
		if created {
			t.Fatal("same-key retry duplicated message")
		}
		must(db.BindTurnToProvider(ctx, turnID, "PRIVATE_PROVIDER_"+turnID, now))
		row, err := db.GetStory(ctx, story)
		must(err)
		_, _, err = db.ApplyStoryEvent(ctx, row, domain.StoryEvent{ID: id, StoryID: story, Kind: "message", RecipientSessionID: manager.ID, Text: text, DeliveryKind: "turn", Reference: turnID, Result: "delivered", CreatedAt: now}, `{}`)
		must(err)
	}
	seed("test-60f4-b1", exchange, turn, original)
	handler := func() http.Handler {
		return httpd.NewRouterWithControl(config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, httpd.APIDeps{Stories: &storysvc.Service{Store: db}}, httpd.ControlDeps{})
	}
	read := func(id string) controllers.StoryDetailResponse {
		t.Helper()
		rec := httptest.NewRecorder()
		handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/workspace/stories/"+id, nil))
		if rec.Code != 200 {
			t.Fatalf("status=%d %s", rec.Code, rec.Body.String())
		}
		for _, sentinel := range []string{"PRIVATE_REASONING", "PRIVATE_RAW", "PRIVATE_PROVIDER", "OTHER_STORY_REPLY"} {
			if id == "test-60f4-b1" && strings.Contains(rec.Body.String(), sentinel) {
				t.Fatalf("leaked %s: %s", sentinel, rec.Body.String())
			}
		}
		var result controllers.StoryDetailResponse
		must(json.Unmarshal(rec.Body.Bytes(), &result))
		return result
	}
	if got := read("test-60f4-b1"); len(got.ReplyThreads) != 1 || len(got.ReplyThreads[0].Messages) != 0 {
		t.Fatalf("before reply: %+v", got.ReplyThreads)
	}
	must(db.UpsertActivity(ctx, conversation.ID, "PRIVATE_PROVIDER_"+turn, domain.ConversationActivity{ID: "reasoning", Kind: domain.ActivityKindReasoning, Status: domain.ActivityStatusCompleted, Summary: "PRIVATE_REASONING", Detail: json.RawMessage(`{"provider":"PRIVATE_RAW"}`)}, now))
	must(db.AppendAssistantDelta(ctx, conversation.ID, "PRIVATE_PROVIDER_ITEM", "PRIVATE_PROVIDER_"+turn, "Test reply", reply, now.Add(3*time.Second)))
	first := read("test-60f4-b1").ReplyThreads[0].Messages
	if len(first) != 1 || first[0].ID != reply || first[0].Text != "Test reply" || !first[0].Streaming {
		t.Fatalf("streaming reply: %+v", first)
	}
	must(db.AppendAssistantDelta(ctx, conversation.ID, "PRIVATE_PROVIDER_ITEM", "PRIVATE_PROVIDER_"+turn, " received", reply, now.Add(4*time.Second)))
	must(db.SettleTurn(ctx, conversation.ID, "PRIVATE_PROVIDER_"+turn, domain.TurnStateCompleted, "", now.Add(4*time.Second)))
	seed("other-story", "other-exchange", "other-turn", "other request")
	must(db.AppendAssistantDelta(ctx, conversation.ID, "other-item", "PRIVATE_PROVIDER_other-turn", "OTHER_STORY_REPLY", "other-reply", now.Add(5*time.Second)))
	got := read("test-60f4-b1")
	messages := got.ReplyThreads[0].Messages
	if len(messages) != 1 || messages[0].Text != "Test reply received" || messages[0].ID != reply || messages[0].TurnID != turn || messages[0].ConversationID != conversation.ID || messages[0].SenderSessionID != manager.ID || messages[0].RecipientSessionID != "" || messages[0].Streaming || !messages[0].CreatedAt.Equal(now.Add(3*time.Second)) || !messages[0].UpdatedAt.Equal(now.Add(4*time.Second)) {
		t.Fatalf("reply identity/content: %+v", messages)
	}
	if got.Story.Status != "in_progress" || len(got.Events) != 1 {
		t.Fatalf("reply mutated workflow/events: %+v", got)
	}
	other := read("other-story")
	if len(other.ReplyThreads) != 1 || len(other.ReplyThreads[0].Messages) != 1 || other.ReplyThreads[0].Messages[0].Text != "OTHER_STORY_REPLY" {
		t.Fatalf("other story attribution: %+v", other.ReplyThreads)
	}
	// A forged reference in a different story cannot borrow another exchange's
	// turn, even when the same manager participates in both stories.
	row, err := db.GetStory(ctx, "other-story")
	must(err)
	_, _, err = db.ApplyStoryEvent(ctx, row, domain.StoryEvent{ID: "wrong-client-key", StoryID: "other-story", Kind: "message", RecipientSessionID: manager.ID, DeliveryKind: "turn", Reference: turn, Result: "delivered", CreatedAt: now}, `{}`)
	must(err)
	for _, thread := range read("other-story").ReplyThreads {
		if thread.ExchangeID == "wrong-client-key" && (thread.Attribution != "unavailable" || len(thread.Messages) != 0) {
			t.Fatalf("reference without original client key leaked: %+v", thread)
		}
	}
	// Reopen must reconstruct the same source IDs, not create copied reply events.
	must(db.Close())
	db, err = sqlite.Open(dir)
	must(err)
	reopened := read("test-60f4-b1")
	if len(reopened.ReplyThreads[0].Messages) != 1 || reopened.ReplyThreads[0].Messages[0].ID != reply {
		t.Fatalf("reopen reply: %+v", reopened)
	}
	// A pending steering reservation may have reached the provider without a
	// receipt. Withhold, then restore only when definitive rejection is recorded.
	_, _, err = db.ReserveSteerDelivery(ctx, conversation.ID, "uncertain-steer", `{}`, now)
	must(err)
	limited := read("test-60f4-b1").ReplyThreads[0]
	if limited.Attribution != "unavailable" || len(limited.Messages) != 0 {
		t.Fatalf("uncertain steer leaked: %+v", limited)
	}
	must(db.RejectSteerDelivery(ctx, conversation.ID, "uncertain-steer", domain.ConversationSteerRejectionKind("provider_refused"), "PRIVATE_RAW", now))
	if len(read("test-60f4-b1").ReplyThreads[0].Messages) != 1 {
		t.Fatal("definitive rejection suppressed unrelated reply")
	}
	// Another story steered the SAME provider turn: neither story may claim its
	// assistant output, even though the original normal-send key still matches.
	_, _, err = db.ReserveSteerDelivery(ctx, conversation.ID, "story-steer", `{}`, now)
	must(err)
	must(db.CompleteSteerDelivery(ctx, conversation.ID, "story-steer", "PRIVATE_PROVIDER_"+turn, domain.ConversationActivity{ID: "steer-activity", Kind: domain.ActivityKindSystem, Status: domain.ActivityStatusCompleted, Summary: "steered", Detail: json.RawMessage(`{"raw":"PRIVATE_RAW"}`)}, now))
	row, err = db.GetStory(ctx, "other-story")
	must(err)
	_, _, err = db.ApplyStoryEvent(ctx, row, domain.StoryEvent{ID: "steer", StoryID: "other-story", Kind: "message", RecipientSessionID: manager.ID, DeliveryKind: "steer", Reference: "steer-activity", Result: "delivered", CreatedAt: now}, `{}`)
	must(err)
	limited = read("test-60f4-b1").ReplyThreads[0]
	if limited.Attribution != "unavailable" || len(limited.Messages) != 0 {
		t.Fatalf("shared normal turn leaked: %+v", limited)
	}
	for _, thread := range read("other-story").ReplyThreads {
		if thread.ExchangeID == "other-exchange" && len(thread.Messages) != 1 {
			t.Fatalf("unrelated provider turn suppressed: %+v", thread)
		}
		if thread.ExchangeID == "steer" && (thread.Attribution != "unavailable" || len(thread.Messages) != 0) {
			t.Fatalf("steer claimed shared reply: %+v", thread)
		}
	}
}
