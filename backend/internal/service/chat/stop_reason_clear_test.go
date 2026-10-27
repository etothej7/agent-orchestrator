package chat_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
)

// Every new controller start claims a generation, and that is the moment the
// session runs again: a stop reason recorded when the agent was last stopped on
// purpose must not outlive it, or a later crash would look like another stop and
// auto-resume would leave the session down.
func TestStartingANewControllerClearsTheStopReason(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := h.st.SetStopReason(ctx, testSession, domain.StopReasonUser, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.st.StopReason(ctx, testSession); got != domain.StopReasonUser {
		t.Fatalf("setup: stop reason = %q", got)
	}
	if err := h.svc.Stop(ctx, testSession); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if got, _ := h.st.StopReason(ctx, testSession); got != domain.StopReasonUser {
		t.Fatalf("stopping the controller must not clear the reason it was stopped for: %q", got)
	}

	if _, err := h.svc.Start(ctx, chatsvc.StartConfig{
		SessionID: testSession, ProjectID: testProject, Harness: domain.HarnessCodex, WorkspacePath: t.TempDir(),
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = h.svc.Stop(context.Background(), testSession) })
	if got, err := h.st.StopReason(ctx, testSession); err != nil || got != domain.StopReasonNone {
		t.Fatalf("stop reason after a new controller started = %q err=%v, want it cleared", got, err)
	}
}
