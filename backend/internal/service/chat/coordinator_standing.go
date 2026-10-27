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
	return fmt.Sprintf("AO standing instruction update (revision %s). This is internal and applies from now on, in addition to your launch instructions. Do not reply about it. Apply these rules to all future work:\n\n%s", revision, domain.RepoManagerStandingText())
}

// DeliverManagerStandingInstructions teaches a running repo manager the
// end-turn rule in place, once per rule revision, but only a manager assigned the
// manager role on a story still in autonomous progress: those are the managers
// whose turns the rule governs. A manager Ed drives by hand, or one with no
// story, gets no extra turn or message, and a manager that is assigned later is
// picked up when the assignment changes. A manager started after the rule shipped
// has it in its launch instructions. It reports whether every qualifying manager
// has the rule or has nothing to wait for.
func (s *Service) DeliverManagerStandingInstructions(ctx context.Context) (bool, error) {
	store, ok := s.store.(coordinatorHumanStore)
	if !ok || s.sessions == nil {
		return true, nil
	}
	stories, ok := s.store.(interface {
		ListStories(context.Context) ([]domain.Story, error)
	})
	if !ok {
		return true, nil
	}
	cfg, err := store.GetCoordination(ctx)
	if err != nil || cfg.CoordinatorProjectID == "" {
		return true, err
	}
	all, err := stories.ListStories(ctx)
	if err != nil {
		return false, err
	}
	handle := ManagerStandingMessageID(domain.RepoManagerStandingRevision())
	delivered := true
	var failures []error
	seen := map[domain.SessionID]bool{}
	for _, story := range all {
		if !story.Autonomous || story.Status == "done" {
			continue
		}
		for _, assignment := range story.Assignments {
			if assignment.Role != "manager" || seen[assignment.SessionID] {
				continue
			}
			seen[assignment.SessionID] = true
			target, found, err := s.sessions.GetSession(ctx, assignment.SessionID)
			if err != nil {
				failures = append(failures, err)
				continue
			}
			if !found || !cfg.Manages(target.ProjectID) || target.Kind != domain.KindOrchestrator || target.IsTerminated || target.Activity.State == domain.ActivityExited || domain.NormalizeSessionMode(target.Mode) != domain.SessionModeChat {
				continue
			}
			conversation, err := s.store.ConversationForSession(ctx, target.ID)
			if errors.Is(err, domain.ErrNoConversation) {
				delivered = false // not provisioned yet; try again later
				continue
			}
			if err != nil {
				failures = append(failures, err)
				continue
			}
			if _, found, err := s.store.ConversationMessageByClientID(ctx, conversation.ID, handle); err != nil {
				failures = append(failures, err)
				continue
			} else if found {
				continue
			}
			if _, err := s.Send(ctx, target.ID, ports.ChatUserMessage{Text: managerStandingText(domain.RepoManagerStandingRevision()), ClientMessageID: handle, Origin: domain.MessageOriginAutomation}); err != nil {
				failures = append(failures, err)
			}
		}
	}
	if err := errors.Join(failures...); err != nil {
		return false, err
	}
	return delivered, nil
}
