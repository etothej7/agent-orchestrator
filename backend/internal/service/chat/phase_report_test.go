package chat_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	reportsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/report"
)

type phaseReportChatDelivery struct{ svc *chatsvc.Service }

func (d phaseReportChatDelivery) Submit(ctx context.Context, id domain.SessionID, text, key string) error {
	_, err := d.svc.Send(ctx, id, ports.ChatUserMessage{Text: text, ClientMessageID: key, Origin: domain.MessageOriginAutomation})
	return err
}
func (d phaseReportChatDelivery) Interrupt(ctx context.Context, id domain.SessionID) error {
	return d.svc.Interrupt(ctx, id)
}

// Real report service/outbox/coordinator and Chat delivery, with only the
// provider doubled. No human message, fake clock advance or RunDue nudge.
func TestPhaseReportsWakeManagerWithoutHumanMessage(t *testing.T) {
	for _, state := range []domain.ReportState{domain.ReportDone, domain.ReportNeedsInput, domain.ReportStuck} {
		t.Run(string(state), func(t *testing.T) {
			h := newHarness(t)
			ctx, cancel := context.WithCancel(context.Background())
			worker, err := h.st.CreateSession(ctx, domain.SessionRecord{ProjectID: testProject, Kind: domain.KindWorker, Harness: domain.HarnessCodex, CreatedAt: h.now(), UpdatedAt: h.now()})
			if err != nil {
				t.Fatal(err)
			}
			coordinator := reportsvc.NewCoordinator(reportsvc.CoordinatorDeps{Store: h.st, Delivery: phaseReportChatDelivery{h.svc}, Now: h.now})
			done := coordinator.Start(ctx)
			t.Cleanup(func() { cancel(); <-done })
			service := reportsvc.New(reportsvc.Deps{Store: h.st, Now: h.now, OnCreated: func(domain.ReportRecord) { coordinator.Wake() }})
			checkpoint, err := service.Create(ctx, reportsvc.CreateInput{SessionID: worker.ID, State: domain.ReportCheckpoint, Note: "Informational checkpoint"})
			if err != nil {
				t.Fatal(err)
			}
			if !checkpoint.AvailableAt.Equal(h.now().Add(time.Hour)) {
				t.Fatal(checkpoint)
			}
			time.Sleep(50 * time.Millisecond)
			if len(h.conv.sentTexts()) != 0 {
				t.Fatal("checkpoint unexpectedly dispatched")
			}
			// A previously interrupted worker must still deliver a new stuck phase.
			if state == domain.ReportStuck {
				if _, _, err := h.st.AcquireReportInterrupt(ctx, worker.ID, h.now(), reportsvc.ReportInterruptWindow); err != nil {
					t.Fatal(err)
				}
			}
			started := time.Now()
			phase, err := service.Create(ctx, reportsvc.CreateInput{SessionID: worker.ID, State: state, Note: "Phase finished; inspect next ready story or review"})
			if err != nil {
				t.Fatal(err)
			}
			if !phase.AvailableAt.Equal(h.now()) {
				t.Fatal("phase report was delayed", phase.AvailableAt)
			}
			deadline := time.Now().Add(2 * time.Second)
			for {
				rec, _, err := h.st.GetReport(ctx, phase.ID)
				if err != nil {
					t.Fatal(err)
				}
				if rec.DeliveryState == domain.ReportAcknowledged && len(h.conv.sentTexts()) == 1 {
					if rec.RecipientSessionID != testSession {
						t.Fatal("wrong manager", rec.RecipientSessionID)
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("phase did not wake manager without human input", rec)
				}
				time.Sleep(5 * time.Millisecond)
			}
			text := h.conv.sentTexts()[0]
			if !strings.Contains(text, phase.Note) || !strings.Contains(text, checkpoint.Note) {
				t.Fatal("phase/checkpoint provenance missing", text)
			}
			snapshot, err := h.st.LoadConversationSnapshot(ctx, h.ctrl.ConversationID())
			if err != nil || len(snapshot.Turns) != 1 || snapshot.Turns[0].ProviderTurnID == "" {
				t.Fatal("no actual provider-bound dispatch", snapshot.Turns, err)
			}
			for _, msg := range snapshot.Messages {
				if msg.Origin == domain.MessageOriginHuman {
					t.Fatal("test needed a human wakeup")
				}
			}
			t.Logf("report %s -> manager turn %s in %s", phase.ID, snapshot.Turns[0].ID, time.Since(started))
		})
	}
}
