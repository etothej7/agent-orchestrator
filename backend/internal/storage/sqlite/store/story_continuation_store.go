package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

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
