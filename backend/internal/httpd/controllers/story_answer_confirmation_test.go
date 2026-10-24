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
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	storysvc "github.com/aoagents/agent-orchestrator/backend/internal/service/story"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

type answerConfirmationMessenger struct {
	calls int
	turn  domain.ConversationTurn
}

func (m *answerConfirmationMessenger) SteerOrSend(context.Context, domain.SessionID, ports.ChatUserMessage, bool) (chatsvc.SteerOrSendResult, error) {
	m.calls++
	return chatsvc.SteerOrSendResult{Turn: m.turn}, nil
}

type beforeConfirmationStore struct {
	storysvc.Store
	before func()
}

func (s *beforeConfirmationStore) ApplyStoryEvent(ctx context.Context, story domain.Story, event domain.StoryEvent, request string) (domain.StoryEvent, bool, error) {
	if event.Kind == "answer_confirmation" && s.before != nil {
		s.before()
		s.before = nil
	}
	return s.Store.ApplyStoryEvent(ctx, story, event, request)
}

func TestStoryHTTPConfirmsOnlyExactExistingDeliveredAnswer(t *testing.T) {
	for _, scenario := range []string{"answer-queued", "answer-failed", "answer-uncertain", "confirm", "wrong-question", "other-requester", "other-story", "before-question", "agent-message", "queued", "failed", "uncertain", "no-reply", "streaming", "running", "wrong-reply", "shared-turn", "proof-changed-before-commit"} {
		t.Run(scenario, func(t *testing.T) {
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
			now := time.Now().UTC()
			must(db.UpsertProject(ctx, domain.ProjectRecord{ID: "repo", Path: t.TempDir(), RegisteredAt: now}))
			manager, err := db.CreateSession(ctx, domain.SessionRecord{ProjectID: "repo", Kind: domain.KindOrchestrator, Harness: domain.HarnessCodex, Mode: domain.SessionModeChat, CreatedAt: now, UpdatedAt: now})
			must(err)
			other, err := db.CreateSession(ctx, domain.SessionRecord{ProjectID: "repo", Kind: domain.KindWorker, Harness: domain.HarnessCodex, Mode: domain.SessionModeChat, CreatedAt: now, UpdatedAt: now})
			must(err)
			conv, err := db.CreateConversation(ctx, "conversation", domain.ConversationScopeProject, "repo", manager.ID, now)
			must(err)
			must(db.ClaimChatControllerGeneration(ctx, manager.ID, "gen"))
			must(db.CreateEpic(ctx, domain.Epic{ID: "epic", Title: "TEST"}))
			for _, id := range []string{"s", "other"} {
				must(db.CreateStory(ctx, domain.Story{ID: id, EpicID: "epic", Title: "TEST", Status: "in_progress", Revision: 1, Assignments: []domain.StoryAssignment{{SessionID: manager.ID, Role: "manager"}, {SessionID: other.ID, Role: "worker"}}}))
			}
			messenger := &answerConfirmationMessenger{}
			wrapped := &beforeConfirmationStore{Store: db}
			svc := &storysvc.Service{Store: wrapped, Messenger: messenger}
			handler := func() http.Handler {
				return httpd.NewRouterWithControl(config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, httpd.APIDeps{Stories: svc}, httpd.ControlDeps{})
			}
			post := func(command storysvc.Command) *httptest.ResponseRecorder {
				t.Helper()
				row, err := db.GetStory(ctx, command.StoryID)
				must(err)
				command.Revision = row.Revision
				body, err := json.Marshal(command)
				must(err)
				rec := httptest.NewRecorder()
				handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/workspace/commands", strings.NewReader(string(body))))
				return rec
			}
			question := func() {
				t.Helper()
				rec := post(storysvc.Command{ID: "question", StoryID: "s", Kind: "question", SenderSessionID: manager.ID, Text: "Blue or Green?"})
				if rec.Code != 200 {
					t.Fatal(rec.Body.String())
				}
			}
			if scenario != "before-question" {
				question()
			}
			if strings.HasPrefix(scenario, "answer-") {
				_, err = db.AppendUserMessage(ctx, conv.ID, manager.ID, "gen", domain.ConversationMessage{ID: "explicit-user", Role: domain.MessageRoleUser, Origin: domain.MessageOriginHuman, Text: "Blue", ClientMessageID: "story-explicit-answer", CreatedAt: now}, "explicit-turn", now)
				must(err)
				if scenario == "answer-failed" {
					must(db.SettleTurnByID(ctx, "explicit-turn", domain.TurnStateFailed, "refused", now))
				}
				if scenario == "answer-uncertain" {
					must(db.MarkTurnDispatching(ctx, "explicit-turn"))
				}
				messenger.turn, err = db.TurnByID(ctx, "explicit-turn")
				must(err)
				command := storysvc.Command{ID: "explicit-answer", StoryID: "s", Kind: "answer", QuestionEventID: "stale-question", Text: "Blue", RecipientSessionID: other.ID}
				if rec := post(command); rec.Code != 400 {
					t.Fatalf("stale question: %d %s", rec.Code, rec.Body.String())
				}
				if messenger.calls != 0 {
					t.Fatal("stale answer dispatched")
				}
				command.QuestionEventID = "question"
				rec := post(command)
				if rec.Code != 200 {
					t.Fatal(rec.Body.String())
				}
				var receipt controllers.StoryEventResponse
				must(json.Unmarshal(rec.Body.Bytes(), &receipt))
				if receipt.Event.RecipientSessionID != manager.ID || receipt.Event.RelatedEventID != "question" || receipt.Event.Result != strings.TrimPrefix(scenario, "answer-") {
					t.Fatalf("receipt: %+v", receipt.Event)
				}
				row, err := db.GetStory(ctx, "s")
				must(err)
				if row.Question == "" {
					t.Fatal("pending or failed answer resolved question")
				}
				if scenario != "answer-queued" {
					return
				}
				must(db.SettleTurnByID(ctx, "explicit-turn", domain.TurnStateCompleted, "", now))
				board := httptest.NewRecorder()
				handler().ServeHTTP(board, httptest.NewRequest(http.MethodGet, "/api/v1/workspace", nil))
				if board.Code != 200 {
					t.Fatal(board.Body.String())
				}
				row, err = db.GetStory(ctx, "s")
				must(err)
				if row.Question != "" || row.Status != "in_progress" {
					t.Fatalf("board did not settle delivery: %+v", row)
				}
				must(db.Close())
				db, err = sqlite.Open(dir)
				must(err)
				wrapped.Store = db
				row, err = db.GetStory(ctx, "s")
				must(err)
				if row.Question != "" {
					t.Fatal("question returned after reopen")
				}
				if rec = post(storysvc.Command{ID: "next-question", StoryID: "s", Kind: "question", SenderSessionID: other.ID, Text: "Next?"}); rec.Code != 200 {
					t.Fatal(rec.Body.String())
				}
				calls := messenger.calls
				if rec = post(command); rec.Code != 200 {
					t.Fatal(rec.Body.String())
				}
				command.QuestionEventID = ""
				if rec = post(command); rec.Code != 200 {
					t.Fatal("legacy retry", rec.Body.String())
				}
				command.QuestionEventID = "next-question"
				if rec = post(command); rec.Code != 409 {
					t.Fatalf("retargeted retry: %d", rec.Code)
				}
				if messenger.calls != calls {
					t.Fatal("settled retry redispatched")
				}
				row, err = db.GetStory(ctx, "s")
				must(err)
				if row.QuestionEventID != "next-question" {
					t.Fatal("retry cleared newer question")
				}
				return
			}
			const exchange = "7e690c5d-ee8a-401d-aab8-b72f768a2476"
			const turn = "35c64bb8-1a2d-487c-8e2c-fa5d62dfcac5"
			const reply = "81363f2a-8436-40f7-94f4-095d1742e651"
			sender := domain.SessionID("")
			if scenario == "agent-message" {
				sender = manager.ID
			}
			_, err = db.AppendUserMessage(ctx, conv.ID, manager.ID, "gen", domain.ConversationMessage{ID: "original-blue", Role: domain.MessageRoleUser, Origin: domain.MessageOriginHuman, SenderSessionID: string(sender), Text: "blue", ClientMessageID: "story-" + exchange, CreatedAt: now}, turn, now)
			must(err)
			must(db.BindTurnToProvider(ctx, turn, "PRIVATE_PROVIDER", now))
			must(db.UpsertActivity(ctx, conv.ID, "PRIVATE_PROVIDER", domain.ConversationActivity{ID: "private", Kind: domain.ActivityKindReasoning, Status: domain.ActivityStatusCompleted, Summary: "PRIVATE_REASONING", Detail: json.RawMessage(`{"payload":"PRIVATE_RAW"}`)}, now))
			if scenario != "no-reply" {
				must(db.AppendAssistantDelta(ctx, conv.ID, "PRIVATE_ITEM", "PRIVATE_PROVIDER", "Blue acknowledged", reply, now))
			}
			if scenario == "running" {
				must(db.SettleAssistantMessage(ctx, conv.ID, "PRIVATE_ITEM", "PRIVATE_PROVIDER", "Blue acknowledged", reply, now))
			}
			if scenario != "streaming" && scenario != "running" {
				must(db.SettleTurn(ctx, conv.ID, "PRIVATE_PROVIDER", domain.TurnStateCompleted, "", now))
			}
			messenger.turn, err = db.TurnByID(ctx, turn)
			must(err)
			sourceStory := "s"
			if scenario == "other-story" {
				sourceStory = "other"
			}
			// Ordinary message delivery, even with an actual reply, must NOT clear a question.
			rec := post(storysvc.Command{ID: exchange, StoryID: sourceStory, Kind: "message", SenderSessionID: sender, RecipientSessionID: manager.ID, Text: "blue"})
			if rec.Code != 200 {
				t.Fatal(rec.Body.String())
			}
			if scenario == "before-question" {
				question()
			}
			source, err := db.StoryEvents(ctx, sourceStory)
			must(err)
			var original domain.StoryEvent
			for _, event := range source {
				if event.ID == exchange {
					original = event
				}
			}
			if scenario == "other-requester" {
				rec = post(storysvc.Command{ID: "new-question", StoryID: "s", Kind: "question", SenderSessionID: other.ID, Text: "New question"})
				if rec.Code != 200 {
					t.Fatal(rec.Body.String())
				}
			}
			// Exercise each non-delivery state using the normal durable pending-message path.
			if scenario == "queued" || scenario == "failed" || scenario == "uncertain" {
				_, err = db.AppendUserMessage(ctx, conv.ID, manager.ID, "gen", domain.ConversationMessage{ID: "pending-user", Role: domain.MessageRoleUser, Origin: domain.MessageOriginHuman, Text: "blue", ClientMessageID: "story-non-delivery", CreatedAt: now}, "not-delivered", now)
				must(err)
				if scenario == "failed" {
					must(db.SettleTurnByID(ctx, "not-delivered", domain.TurnStateFailed, "rejected", now))
				}
				if scenario == "uncertain" {
					must(db.MarkTurnDispatching(ctx, "not-delivered"))
				}
				messenger.turn, err = db.TurnByID(ctx, "not-delivered")
				must(err)
				rec = post(storysvc.Command{ID: "non-delivery", StoryID: "s", Kind: "message", RecipientSessionID: manager.ID, Text: "blue"})
				if rec.Code != 200 {
					t.Fatal(rec.Body.String())
				}
			}
			reserve := func() { _, _, err := db.ReserveSteerDelivery(ctx, conv.ID, "shared-guidance", `{}`, now); must(err) }
			if scenario == "shared-turn" {
				reserve()
			}
			if scenario == "proof-changed-before-commit" {
				wrapped.before = reserve
			}
			row, err := db.GetStory(ctx, "s")
			must(err)
			if row.Question == "" {
				t.Fatal("ordinary message cleared question")
			}
			command := storysvc.Command{ID: "confirm-original", StoryID: "s", Kind: "answer_confirmation", QuestionEventID: row.QuestionEventID, Reference: exchange, ReplyMessageID: reply}
			if scenario == "wrong-question" {
				command.QuestionEventID = "obsolete-question"
			}
			if scenario == "wrong-reply" {
				command.ReplyMessageID = "other-reply"
			}
			if scenario == "queued" || scenario == "failed" || scenario == "uncertain" {
				command.Reference = "non-delivery"
			}
			calls := messenger.calls
			rec = post(command)
			if scenario != "confirm" {
				if rec.Code != 400 {
					t.Fatalf("expected rejection, HTTP%d %s", rec.Code, rec.Body.String())
				}
				row, err = db.GetStory(ctx, "s")
				must(err)
				if row.Question == "" {
					t.Fatal("invalid confirmation cleared question")
				}
			} else {
				if rec.Code != 200 {
					t.Fatalf("confirm: HTTP%d %s", rec.Code, rec.Body.String())
				}
				var result controllers.StoryEventResponse
				must(json.Unmarshal(rec.Body.Bytes(), &result))
				if result.Event.Kind != "answer_confirmation" || result.Event.Result != "confirmed" || result.Event.RelatedEventID != "question" || result.Event.Reference != exchange || result.Event.ReplyMessageID != reply || result.Event.RecipientSessionID != manager.ID {
					t.Fatalf("audit: %+v", result.Event)
				}
				// Board-only polling sees settlement before detail is opened.
				board := httptest.NewRecorder()
				handler().ServeHTTP(board, httptest.NewRequest(http.MethodGet, "/api/v1/workspace", nil))
				var workspace controllers.WorkspaceResponse
				must(json.Unmarshal(board.Body.Bytes(), &workspace))
				for _, story := range workspace.Stories {
					if story.ID == "s" && (story.Question != "" || story.Status != "in_progress") {
						t.Fatalf("board: %+v", story)
					}
				}
				if retry := post(command); retry.Code != 200 {
					t.Fatalf("retry: %s", retry.Body.String())
				}
				conflicting := command
				conflicting.ReplyMessageID = "different"
				if retry := post(conflicting); retry.Code != 409 {
					t.Fatalf("changed retry: %d", retry.Code)
				}
				must(db.Close())
				db, err = sqlite.Open(dir)
				must(err)
				wrapped.Store = db
				row, err = db.GetStory(ctx, "s")
				must(err)
				if row.Question != "" || row.Status != "in_progress" {
					t.Fatalf("reopen: %+v", row)
				}
			}
			if messenger.calls != calls {
				t.Fatal("confirmation dispatched another message")
			}
			detail := httptest.NewRecorder()
			handler().ServeHTTP(detail, httptest.NewRequest(http.MethodGet, "/api/v1/workspace/stories/s", nil))
			for _, private := range []string{"PRIVATE_PROVIDER", "PRIVATE_ITEM", "PRIVATE_REASONING", "PRIVATE_RAW"} {
				if strings.Contains(detail.Body.String(), private) {
					t.Fatalf("privacy leak %s", private)
				}
			}
			source, err = db.StoryEvents(ctx, sourceStory)
			must(err)
			for _, event := range source {
				if event.ID == exchange && event != original {
					t.Fatalf("original event rewritten: %+v", event)
				}
			}
		})
	}
}
