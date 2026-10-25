package notify

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// Coordinated work wakes its manager, not the human. A real provider permission
// blocker and a merge decision remain actionable; idle and review receipts do not.
func (m *Manager) quietCoordination(ctx context.Context, intent Intent) (bool, error) {
	store, ok := m.store.(interface {
		GetCoordination(context.Context) (domain.CoordinationConfig, error)
		GetSession(context.Context, domain.SessionID) (domain.SessionRecord, bool, error)
		ListStories(context.Context) ([]domain.Story, error)
	})
	if !ok {
		return false, nil
	}
	cfg, err := store.GetCoordination(ctx)
	if err != nil {
		return false, err
	}
	coordinated := cfg.CoordinatorProjectID != "" && cfg.CoordinatorProjectID == intent.ProjectID
	if !coordinated {
		stories, err := store.ListStories(ctx)
		if err != nil {
			return false, err
		}
		for _, story := range stories {
			if !story.Autonomous || story.Status == "done" {
				continue
			}
			for _, assignment := range story.Assignments {
				if assignment.SessionID == intent.SessionID {
					coordinated = true
				}
			}
		}
	}
	if !coordinated {
		return false, nil
	}
	switch intent.Type {
	case domain.NotificationReviewCompleted, domain.NotificationReviewChangesRequested, domain.NotificationPRMerged, domain.NotificationPRClosedUnmerged:
		return true, nil
	case domain.NotificationNeedsInput:
		session, found, err := store.GetSession(ctx, intent.SessionID)
		if err != nil {
			return false, err
		}
		return found && session.Activity.State != domain.ActivityBlocked, nil
	default:
		return false, nil
	}
}
