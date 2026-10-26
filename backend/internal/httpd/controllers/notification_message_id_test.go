package controllers

import (
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// A chat block finds its coordinator notification by message id, not by guessing
// from the text; the internal source key never leaves the daemon.
func TestNotificationResponseCarriesTheCoordinatorMessageIDOnly(t *testing.T) {
	ask := domain.NotificationRecord{ID: "n1", Type: domain.NotificationCoordinatorMessage, Purpose: "scope_question", SourceKey: "coordinator-message:decision-sleep-wording"}
	if got := notificationResponseFromRecord(ask).MessageID; got != "decision-sleep-wording" {
		t.Fatalf("messageId = %q, want the ask's message id", got)
	}
	other := domain.NotificationRecord{ID: "n2", Type: domain.NotificationReviewCompleted, SourceKey: "review:abc", PRURL: "https://example.test/pr/1"}
	if got := notificationResponseFromRecord(other).MessageID; got != "" {
		t.Fatalf("messageId = %q on a non-coordinator notification, want none", got)
	}
}
