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
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	storysvc "github.com/aoagents/agent-orchestrator/backend/internal/service/story"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

type blockedStorySteerer struct {
	*steerRecorder
	entered chan struct{}
	release chan struct{}
}

func (p *blockedStorySteerer) Steer(ctx context.Context, turn string, msg ports.ChatUserMessage) (ports.ChatTurnRef, error) {
	close(p.entered)
	select {
	case <-p.release:
	case <-ctx.Done():
		return ports.ChatTurnRef{}, ctx.Err()
	}
	return p.steerRecorder.Steer(ctx, turn, msg)
}

type failedStorySteerReservation struct{ chatsvc.Store }

func (s *failedStorySteerReservation) ReserveSteerDelivery(context.Context, string, string, string, time.Time) (domain.ConversationSteerDelivery, bool, error) {
	return domain.ConversationSteerDelivery{}, false, errors.New("injected reservation failure")
}

type failedStorySteerRejection struct{ chatsvc.Store }

func (s *failedStorySteerRejection) RejectSteerDelivery(context.Context, string, string, domain.ConversationSteerRejectionKind, string, time.Time) error {
	return errors.New("injected rejection persistence failure")
}

// Exercise the real legacy service/storage paths, which do not create keyed
// steer receipts. The provider alone is simulated; story projection is HTTP.
func TestStoryHTTPWithholdsRepliesAfterUnreceiptedSteering(t *testing.T) {
	for _, mode := range []string{"no-client-key", "promoted", "uncertain-promotion", "reserved-promotion", "rejected-promotion", "legacy-activity", "inflight-keyless", "uncertain-keyless", "refused-keyless", "reservation-failure", "completion-failure", "rejection-failure"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			provider := newSteerRecorder()
			var transport ports.ChatConversation = provider
			blocked := &blockedStorySteerer{steerRecorder: provider, entered: make(chan struct{}), release: make(chan struct{})}
			if mode == "inflight-keyless" {
				transport = blocked
			}
			h := newHarnessWithConversationAndStore(t, transport, func(st *store.Store) chatsvc.Store {
				switch mode {
				case "reservation-failure":
					return &failedStorySteerReservation{Store: st}
				case "rejection-failure":
					return &failedStorySteerRejection{Store: st}
				case "completion-failure":
					return &failSteerCompletionStore{Store: st}
				default:
					return st
				}
			})
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
			if mode == "legacy-activity" {
				// Historical keyless records predate receipts; exercise their actual store path.
				must(h.st.UpsertActivity(ctx, h.ctrl.ConversationID(), "provider-turn-1", domain.ConversationActivity{ID: "legacy", Kind: domain.ActivityKindSystem, Status: domain.ActivityStatusCompleted, Detail: json.RawMessage(`{"event":"steer","text":"PRIVATE_OTHER_STORY"}`)}, h.now()))
			} else if mode == "inflight-keyless" {
				t.Cleanup(func() {
					select {
					case <-blocked.release:
					default:
						close(blocked.release)
					}
				})
				done := make(chan error, 1)
				go func() {
					_, err := h.svc.Steer(ctx, testSession, ports.ChatUserMessage{Text: "PRIVATE_OTHER_STORY"})
					done <- err
				}()
				select {
				case <-blocked.entered:
				case <-time.After(5 * time.Second):
					t.Fatal("provider not entered")
				}
				// Release even if an assertion fails, so controller cleanup cannot deadlock.
				got := read()
				close(blocked.release)
				must(<-done)
				if got.Attribution != "unavailable" || len(got.Messages) != 0 {
					t.Fatalf("in-flight keyless reply leaked: %+v", got)
				}
			} else if mode == "no-client-key" || mode == "uncertain-keyless" || mode == "refused-keyless" || mode == "reservation-failure" || mode == "completion-failure" || mode == "rejection-failure" {
				if mode == "uncertain-keyless" {
					provider.failWith(errors.New("transport lost after write"))
				}
				if mode == "refused-keyless" || mode == "rejection-failure" {
					provider.failWith(ports.ErrChatTurnNotSteerable)
				}
				_, err = h.svc.Steer(ctx, testSession, ports.ChatUserMessage{Text: "PRIVATE_OTHER_STORY guidance"})
				switch mode {
				case "no-client-key":
					must(err)
				case "refused-keyless":
					if !errors.Is(err, chatsvc.ErrTurnNotSteerable) {
						t.Fatalf("refusal: %v", err)
					}
				default:
					if !errors.Is(err, chatsvc.ErrSteerDeliveryUncertain) {
						t.Fatalf("uncertainty: %v", err)
					}
				}
				calls := provider.steers()
				want := 1
				if mode == "reservation-failure" {
					want = 0
				}
				if len(calls) != want {
					t.Fatalf("provider calls=%d want%d", len(calls), want)
				}
				if len(calls) > 0 && calls[0].msg.ClientMessageID != "" {
					t.Fatal("internal receipt changed provider client key")
				}
				if mode == "uncertain-keyless" || mode == "completion-failure" || mode == "rejection-failure" {
					restartSteerService(t, h, provider)
					if len(provider.steers()) != 1 {
						t.Fatal("restart redispatched uncertain guidance")
					}
				}
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
			if mode == "rejected-promotion" || mode == "refused-keyless" || mode == "reservation-failure" {
				if got.Attribution != "turn" || len(got.Messages) != 1 {
					t.Fatalf("released reservation: %+v", got)
				}
			} else if got.Attribution != "unavailable" || len(got.Messages) != 0 {
				t.Fatalf("shared reply leaked after %s: %+v", mode, got)
			}
		})
	}
}
