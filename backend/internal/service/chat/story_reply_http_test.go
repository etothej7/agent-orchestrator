package chat_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	storysvc "github.com/aoagents/agent-orchestrator/backend/internal/service/story"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// Exercise the real legacy service/storage paths, which do not create keyed
// steer receipts. The provider alone is simulated; story projection is HTTP.
func TestStoryHTTPWithholdsRepliesAfterUnreceiptedSteering(t *testing.T) {
	for _, mode := range []string{"no-client-key", "promoted", "uncertain-promotion", "reserved-promotion", "rejected-promotion"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			provider := newSteerRecorder()
			h := newHarnessWithConversation(t, provider)
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			turn, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{Text: "story A", ClientMessageID: "story-exchange-a", Origin: domain.MessageOriginHuman})
			must(err)
			provider.emit(ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: "provider-turn-1"})
			h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
				return len(s.Turns) == 1 && s.Turns[0].ProviderTurnID == "provider-turn-1"
			})
			must(h.st.CreateEpic(ctx, domain.Epic{ID: "epic", Title: "TEST"}))
			must(h.st.CreateStory(ctx, domain.Story{ID: "story-a", EpicID: "epic", Title: "TEST A", Status: "in_progress", Revision: 1}))
			row, err := h.st.GetStory(ctx, "story-a")
			must(err)
			_, _, err = h.st.ApplyStoryEvent(ctx, row, domain.StoryEvent{ID: "exchange-a", StoryID: "story-a", Kind: "message", RecipientSessionID: testSession, DeliveryKind: "turn", Reference: turn.ID, Result: "delivered", CreatedAt: h.now()}, `{}`)
			must(err)
			must(h.st.AppendAssistantDelta(ctx, h.ctrl.ConversationID(), "item", "provider-turn-1", "PUBLIC_REPLY", "reply", h.now()))
			handler := httpd.NewRouterWithControl(config.Config{}, slog.New(slog.DiscardHandler), nil, httpd.APIDeps{Stories: &storysvc.Service{Store: h.st}}, httpd.ControlDeps{})
			read := func() domain.StoryReplyThread {
				t.Helper()
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/workspace/stories/story-a", nil))
				if rec.Code != 200 {
					t.Fatalf("HTTP %d: %s", rec.Code, rec.Body.String())
				}
				if strings.Contains(rec.Body.String(), "PRIVATE_OTHER_STORY") {
					t.Fatal("steer payload leaked")
				}
				var detail controllers.StoryDetailResponse
				must(json.Unmarshal(rec.Body.Bytes(), &detail))
				if len(detail.ReplyThreads) != 1 {
					t.Fatalf("threads: %+v", detail.ReplyThreads)
				}
				return detail.ReplyThreads[0]
			}
			if got := read(); got.Attribution != "turn" || len(got.Messages) != 1 {
				t.Fatalf("unambiguous baseline: %+v", got)
			}
			if mode == "no-client-key" {
				_, err = h.svc.Steer(ctx, testSession, ports.ChatUserMessage{Text: "PRIVATE_OTHER_STORY guidance"})
				must(err)
			} else {
				queued, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{Text: "PRIVATE_OTHER_STORY guidance", ClientMessageID: "story-b-guidance", Origin: domain.MessageOriginHuman})
				must(err)
				// An ordinary queued message has not affected the running provider turn.
				if got := read(); len(got.Messages) != 1 {
					t.Fatalf("queue alone suppressed reply: %+v", got)
				}
				switch mode {
				case "reserved-promotion":
					_, err = h.st.ReserveQueuedTurnForPromotion(ctx, h.ctrl.ConversationID(), queued.ID, h.now())
					must(err)
				case "uncertain-promotion":
					provider.failWith(errors.New("transport lost after write"))
					_, err = h.svc.PromoteQueuedTurn(ctx, testSession, queued.ID)
					if !errors.Is(err, chatsvc.ErrPromotionUncertain) {
						t.Fatalf("uncertain promotion: %v", err)
					}
				case "rejected-promotion":
					// Definitive provider rejection releases the durable reservation.
					provider.failWith(ports.ErrChatTurnNotSteerable)
					_, err = h.svc.PromoteQueuedTurn(ctx, testSession, queued.ID)
					if !errors.Is(err, chatsvc.ErrTurnNotSteerable) {
						t.Fatalf("rejected promotion: %v", err)
					}
				default:
					_, err = h.svc.PromoteQueuedTurn(ctx, testSession, queued.ID)
					must(err)
				}
			}
			got := read()
			if mode == "rejected-promotion" {
				if got.Attribution != "turn" || len(got.Messages) != 1 {
					t.Fatalf("released reservation: %+v", got)
				}
			} else if got.Attribution != "unavailable" || len(got.Messages) != 0 {
				t.Fatalf("shared reply leaked after %s: %+v", mode, got)
			}
		})
	}
}
