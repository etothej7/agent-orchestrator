package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// CreateStoryIntake binds a human request to its epic and stories in one transaction.
func (s *Store) CreateStoryIntake(ctx context.Context, id, sourceID string, actor domain.SessionID, hash string, epic domain.Epic, stories []domain.Story) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.inTx(ctx, "adopt story intake", func(q *gen.Queries) error {
		existing, err := q.GetStoryIntake(ctx, gen.GetStoryIntakeParams{ID: id, SourceMessageID: sourceID})
		if err == nil {
			if existing.ID == id && existing.SourceMessageID == sourceID && existing.SenderSessionID == string(actor) && existing.RequestHash == hash {
				return nil
			}
			return apierr.Conflict("INTAKE_ALREADY_ADOPTED", "The request or intake id already belongs to a plan; inspect and continue that plan", map[string]any{"intakeId": existing.ID, "epicId": existing.EpicID})
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		source, err := q.GetStoryIntakeSource(ctx, gen.GetStoryIntakeSourceParams{ID: sourceID, CurrentSessionID: &actor})
		if errors.Is(err, sql.ErrNoRows) {
			return apierr.Invalid("INVALID_INTAKE_SOURCE", "Choose a human message in the current coordinator conversation", nil)
		}
		if err != nil {
			return err
		}
		if source.Role != domain.MessageRoleUser || source.Origin != domain.MessageOriginHuman || source.SenderSessionID != "" {
			return apierr.Invalid("INVALID_INTAKE_SOURCE", "Agent reports and replies are not human authorization", nil)
		}
		if err := q.CreateWorkspaceEpic(ctx, gen.CreateWorkspaceEpicParams{ID: epic.ID, Title: epic.Title}); err != nil {
			return err
		}
		for _, story := range stories {
			if err := q.CreateWorkspaceStory(ctx, gen.CreateWorkspaceStoryParams{ID: story.ID, EpicID: story.EpicID, Record: storyJSON(story)}); err != nil {
				return err
			}
		}
		return q.InsertStoryIntake(ctx, gen.InsertStoryIntakeParams{ID: id, SourceMessageID: sourceID, SenderSessionID: string(actor), RequestHash: hash, EpicID: epic.ID})
	})
}
