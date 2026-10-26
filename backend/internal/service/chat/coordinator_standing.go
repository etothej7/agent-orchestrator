package chat

import (
	"context"
	"fmt"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// CoordinatorStandingMessageID is the stable delivery handle for one revision
// of the coordinator's human-contact rules.
func CoordinatorStandingMessageID(revision string) string {
	return "ao-standing-coordinator-" + revision
}

func coordinatorStandingText(revision string) string {
	return fmt.Sprintf("AO standing instruction update (revision %s). This is internal and applies from now on, in addition to your launch instructions. Do not reply to Ed about it, and do not use ao ask or ao tell for it. Apply these rules to all future work:\n\n%s", revision, domain.CoordinatorHumanContactRules)
}

// DeliverCoordinatorStandingInstructions teaches an already-running overall
// coordinator the current human-contact rules without replacing it. Delivery is
// one durable automation message per rule revision, so its turn stays internal,
// a retry after an uncertain send reuses the same handle, and nothing is ever
// resent once the message exists. It reports whether the revision is delivered.
func (s *Service) DeliverCoordinatorStandingInstructions(ctx context.Context) (bool, error) {
	store, ok := s.store.(coordinatorHumanStore)
	if !ok {
		return true, nil
	}
	lister, ok := s.store.(interface {
		ListSessions(context.Context, domain.ProjectID) ([]domain.SessionRecord, error)
	})
	if !ok {
		return true, nil
	}
	cfg, err := store.GetCoordination(ctx)
	if err != nil || cfg.CoordinatorProjectID == "" {
		return true, err
	}
	sessions, err := lister.ListSessions(ctx, cfg.CoordinatorProjectID)
	if err != nil {
		return false, err
	}
	var target domain.SessionRecord
	for _, rec := range sessions {
		if rec.Kind == domain.KindOrchestrator && !rec.IsTerminated && rec.Activity.State != domain.ActivityExited && domain.NormalizeSessionMode(rec.Mode) == domain.SessionModeChat && (target.ID == "" || rec.CreatedAt.After(target.CreatedAt)) {
			target = rec
		}
	}
	if target.ID == "" {
		return false, nil
	}
	conversation, err := s.store.ConversationForSession(ctx, target.ID)
	if err != nil {
		return false, nil // not provisioned yet; try again later
	}
	revision := domain.CoordinatorStandingRevision()
	handle := CoordinatorStandingMessageID(revision)
	if _, found, err := s.store.ConversationMessageByClientID(ctx, conversation.ID, handle); err != nil || found {
		return found, err
	}
	if strings.TrimSpace(handle) == "" {
		return false, nil
	}
	if _, err := s.Send(ctx, target.ID, ports.ChatUserMessage{Text: coordinatorStandingText(revision), ClientMessageID: handle, Origin: domain.MessageOriginAutomation}); err != nil {
		return false, err
	}
	return true, nil
}
