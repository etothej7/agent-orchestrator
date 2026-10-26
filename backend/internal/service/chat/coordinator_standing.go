package chat

import (
	"context"
	"errors"
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
	return fmt.Sprintf("AO standing instruction update (revision %s). This is internal and applies from now on, in addition to your launch instructions. Do not reply to Ed about it, and do not use ao ask or ao tell for it. Apply these rules to all future work:\n\n%s", revision, domain.CoordinatorStandingText())
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
	if errors.Is(err, domain.ErrNoConversation) {
		return false, nil // not provisioned yet; try again later
	}
	if err != nil {
		return false, err
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

// ManagerStandingMessageID is the stable delivery handle for one revision of the
// repo-manager end-turn rule.
func ManagerStandingMessageID(revision string) string {
	return "ao-standing-manager-" + revision
}

func managerStandingText(revision string) string {
	return fmt.Sprintf("AO standing instruction update (revision %s). This is internal and applies from now on, in addition to your launch instructions. Do not reply about it. Apply this rule to all future work:\n\n%s", revision, domain.RepoManagerEndTurnRule)
}

// DeliverManagerStandingInstructions teaches each already-running repo manager
// the end-turn rule without replacing it, once per rule revision. A project with
// no live manager has nothing to teach: a manager started later receives the rule
// in its launch instructions. It reports whether every live manager has it.
func (s *Service) DeliverManagerStandingInstructions(ctx context.Context) (bool, error) {
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
	handle := ManagerStandingMessageID(domain.RepoManagerStandingRevision())
	delivered := true
	for _, project := range cfg.ManagedProjectIDs {
		sessions, err := lister.ListSessions(ctx, project)
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
			continue
		}
		conversation, err := s.store.ConversationForSession(ctx, target.ID)
		if errors.Is(err, domain.ErrNoConversation) {
			delivered = false // not provisioned yet; try again later
			continue
		}
		if err != nil {
			return false, err
		}
		if _, found, err := s.store.ConversationMessageByClientID(ctx, conversation.ID, handle); err != nil {
			return false, err
		} else if found {
			continue
		}
		if _, err := s.Send(ctx, target.ID, ports.ChatUserMessage{Text: managerStandingText(domain.RepoManagerStandingRevision()), ClientMessageID: handle, Origin: domain.MessageOriginAutomation}); err != nil {
			return false, err
		}
	}
	return delivered, nil
}

// DeliverStandingInstructions teaches every running coordinator and repo
// manager its current rules in place. It reports whether everything that is
// running has them, so the caller can stop retrying.
func (s *Service) DeliverStandingInstructions(ctx context.Context) (bool, error) {
	coordinator, coordinatorErr := s.DeliverCoordinatorStandingInstructions(ctx)
	managers, managerErr := s.DeliverManagerStandingInstructions(ctx)
	if err := errors.Join(coordinatorErr, managerErr); err != nil {
		return false, err
	}
	return coordinator && managers, nil
}
