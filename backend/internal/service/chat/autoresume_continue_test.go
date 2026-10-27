package chat_test

import (
	"context"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/autoresume"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// The auto-resume continue turn is sent through the real chat service by its stable
// client message id: a repeated sweep, a second boot of the same intent or a replay
// returns the recorded turn, the provider hears it once, and it is hidden
// automation rather than a message Ed appears to have written.
func TestAutoResumeContinueTurnIsDeliveredOnceAndIsAutomation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sender := autoresume.ContinueSender{Chat: h.svc}

	for range 3 {
		if err := sender.SendContinue(ctx, testSession, "boot-1"); err != nil {
			t.Fatalf("SendContinue: %v", err)
		}
	}

	if sent := h.conv.sentMessages(); len(sent) != 1 || sent[0].Text != autoresume.ContinueText {
		t.Fatalf("provider received %d messages %+v, want the continue turn exactly once", len(sent), sent)
	}
	snapshot := h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool { return len(s.Messages) == 1 })
	if len(snapshot.Turns) != 1 || len(snapshot.Messages) != 1 {
		t.Fatalf("snapshot has %d turns and %d messages, want one of each", len(snapshot.Turns), len(snapshot.Messages))
	}
	if got := snapshot.Messages[0]; got.Origin != domain.MessageOriginAutomation ||
		got.ClientMessageID != autoresume.ContinueMessageID(testSession, "boot-1") {
		t.Fatalf("message = origin %q key %q, want hidden automation with the stable key", got.Origin, got.ClientMessageID)
	}

	// Another boot is a new intent and so a new turn.
	if err := sender.SendContinue(ctx, testSession, "boot-2"); err != nil {
		t.Fatal(err)
	}
	// It queues behind the turn still running, so assert on what is stored.
	snapshot = h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool { return len(s.Messages) == 2 })
	if snapshot.Messages[1].ClientMessageID != autoresume.ContinueMessageID(testSession, "boot-2") {
		t.Fatalf("a different boot must record its own continue turn: %+v", snapshot.Messages)
	}
}
