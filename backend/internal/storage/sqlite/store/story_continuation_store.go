package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// GetStoryContinuation reads the durable dispatch and recovery state.
func (s *Store) GetStoryContinuation(ctx context.Context, id string) (domain.StoryContinuation, bool, error) {
	row, err := s.qr.GetStoryContinuation(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.StoryContinuation{}, false, nil
	}
	if err != nil {
		return domain.StoryContinuation{}, false, err
	}
	var rec domain.StoryContinuation
	if err := json.Unmarshal([]byte(row.Record), &rec); err != nil {
		return rec, false, err
	}
	rec.Revision = row.Revision
	return rec, true, nil
}

// SaveStoryContinuation is compare-and-swap, including the first claim. A lost
// claim cannot send, settle, or overwrite a newer owner's receipt.
func (s *Store) SaveStoryContinuation(ctx context.Context, rec domain.StoryContinuation) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var n int64
	var err error
	if rec.Revision == 0 {
		n, err = s.qw.InsertStoryContinuation(ctx, gen.InsertStoryContinuationParams{StoryID: rec.StoryID, Record: storyJSON(rec)})
	} else {
		n, err = s.qw.UpdateStoryContinuation(ctx, gen.UpdateStoryContinuationParams{StoryID: rec.StoryID, Record: storyJSON(rec), Revision: rec.Revision})
	}
	return n == 1, err
}

// ClaimStoryContinuation linearizes dispatch eligibility with the ledger claim.
// A question, completion or assignment edit committed after the sweep read wins.
func (s *Store) ClaimStoryContinuation(ctx context.Context, rec domain.StoryContinuation, storyRevision int64) (bool, error) {
	return s.writeEligibleStoryContinuation(ctx, rec, storyRevision, nil)
}

// ResumeStoryContinuation commits the bounded retry and its public audit together.
func (s *Store) ResumeStoryContinuation(ctx context.Context, rec domain.StoryContinuation, storyRevision int64, audit domain.StoryEvent) (bool, error) {
	return s.writeEligibleStoryContinuation(ctx, rec, storyRevision, &audit)
}

func (s *Store) writeEligibleStoryContinuation(ctx context.Context, rec domain.StoryContinuation, storyRevision int64, audit *domain.StoryEvent) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var n int64
	err := s.inTx(ctx, "claim eligible story continuation", func(q *gen.Queries) error {
		row, err := q.GetWorkspaceStory(ctx, rec.StoryID)
		if err != nil {
			return err
		}
		var story domain.Story
		if err := json.Unmarshal([]byte(row.Record), &story); err != nil {
			return err
		}
		if row.Revision != storyRevision || !story.Autonomous || story.Status == "done" || story.Question != "" {
			return nil
		}
		if rec.Revision == 0 {
			n, err = q.InsertStoryContinuation(ctx, gen.InsertStoryContinuationParams{StoryID: rec.StoryID, Record: storyJSON(rec)})
		} else {
			n, err = q.UpdateStoryContinuation(ctx, gen.UpdateStoryContinuationParams{StoryID: rec.StoryID, Record: storyJSON(rec), Revision: rec.Revision})
		}
		if err != nil || n != 1 || audit == nil {
			return err
		}
		return q.CreateWorkspaceEvent(ctx, gen.CreateWorkspaceEventParams{ID: audit.ID, StoryID: audit.StoryID, Record: storyJSON(audit), Request: storyJSON(audit)})
	})
	return n == 1, err
}
