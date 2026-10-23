package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

func storyJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

// ListEpics returns the shared durable grouping list.
func (s *Store) ListEpics(ctx context.Context) ([]domain.Epic, error) {
	rows, err := s.qr.ListWorkspaceEpics(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]domain.Epic, 0, len(rows))
	for _, r := range rows {
		out = append(out, domain.Epic{ID: r.ID, Title: r.Title})
	}
	return out, nil
}

// CreateEpic accepts identical retries without replacing an existing title.
func (s *Store) CreateEpic(ctx context.Context, e domain.Epic) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.ListWorkspaceEpics(ctx)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if r.ID == e.ID {
			if r.Title == e.Title {
				return nil
			}
			return apierr.Conflict("EPIC_EXISTS", "Epic id already exists", nil)
		}
	}
	return s.qw.CreateWorkspaceEpic(ctx, gen.CreateWorkspaceEpicParams{ID: e.ID, Title: e.Title})
}

// CreateStory stores the initial backlog; no session state is changed.
func (s *Store) CreateStory(ctx context.Context, story domain.Story) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	old, err := s.qw.GetWorkspaceStory(ctx, story.ID)
	if err == nil {
		if old.Record == storyJSON(story) {
			return nil
		}
		return apierr.Conflict("STORY_EXISTS", "Story id already exists", nil)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return s.qw.CreateWorkspaceStory(ctx, gen.CreateWorkspaceStoryParams{ID: story.ID, EpicID: story.EpicID, Record: storyJSON(story)})
}

// GetStory returns one durable story.
func (s *Store) GetStory(ctx context.Context, id string) (domain.Story, error) {
	var story domain.Story
	r, err := s.qr.GetWorkspaceStory(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return story, apierr.NotFound("STORY_NOT_FOUND", "Unknown story")
	}
	if err != nil {
		return story, err
	}
	err = json.Unmarshal([]byte(r.Record), &story)
	story.Revision = r.Revision
	return story, err
}

// ListStories returns stories in creation order.
func (s *Store) ListStories(ctx context.Context) ([]domain.Story, error) {
	rows, err := s.qr.ListWorkspaceStories(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]domain.Story, 0, len(rows))
	for _, r := range rows {
		var story domain.Story
		if err := json.Unmarshal([]byte(r.Record), &story); err != nil {
			return nil, err
		}
		story.Revision = r.Revision
		out = append(out, story)
	}
	return out, nil
}

// StoryEvents contains only explicit public events and references.
func (s *Store) StoryEvents(ctx context.Context, id string) ([]domain.StoryEvent, error) {
	rows, err := s.qr.ListWorkspaceEvents(ctx, id)
	if err != nil {
		return nil, err
	}
	out := make([]domain.StoryEvent, 0, len(rows))
	for _, r := range rows {
		var e domain.StoryEvent
		if err := json.Unmarshal([]byte(r.Record), &e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

// ApplyStoryEvent commits the decision and its event together under an optimistic revision fence.
func (s *Store) ApplyStoryEvent(ctx context.Context, story domain.Story, e domain.StoryEvent, request string) (domain.StoryEvent, bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	duplicate := false
	err := s.inTx(ctx, "story event", func(q *gen.Queries) error {
		old, err := q.GetWorkspaceEvent(ctx, e.ID)
		if err == nil {
			if old.StoryID != story.ID || old.Request != request {
				return apierr.Conflict("STORY_KEY_REUSED", "Event id belongs to a different request", nil)
			}
			duplicate = true
			return json.Unmarshal([]byte(old.Record), &e)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		revision := story.Revision
		story.Revision++
		count, err := q.UpdateWorkspaceStory(ctx, gen.UpdateWorkspaceStoryParams{ID: story.ID, Record: storyJSON(story), Revision: revision})
		if err != nil {
			return err
		}
		if count != 1 {
			return apierr.Conflict("STORY_CHANGED", "Story changed; reload before making this decision", nil)
		}
		return q.CreateWorkspaceEvent(ctx, gen.CreateWorkspaceEventParams{ID: e.ID, StoryID: e.StoryID, Record: storyJSON(e), Request: request})
	})
	return e, duplicate, err
}

// FinishStoryDelivery updates a receipt without changing the story's workflow status.
func (s *Store) FinishStoryDelivery(ctx context.Context, e domain.StoryEvent) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.inTx(ctx, "story delivery", func(q *gen.Queries) error {
		old, err := q.GetWorkspaceEvent(ctx, e.ID)
		if err != nil {
			return err
		}
		var current domain.StoryEvent
		if err := json.Unmarshal([]byte(old.Record), &current); err != nil {
			return err
		}
		if current.Result == "delivered" || current.Result == "failed" {
			return nil
		}
		if err := q.UpdateWorkspaceEvent(ctx, gen.UpdateWorkspaceEventParams{ID: e.ID, Record: storyJSON(e)}); err != nil {
			return err
		}
		if e.Kind == "answer" && e.Result == "delivered" {
			row, err := q.GetWorkspaceStory(ctx, e.StoryID)
			if err != nil {
				return err
			}
			var story domain.Story
			if err := json.Unmarshal([]byte(row.Record), &story); err != nil {
				return err
			}
			if story.QuestionEventID == e.RelatedEventID {
				story.Question = ""
				story.QuestionEventID = ""
				story.QuestionRequesterSessionID = ""
				story.Revision = row.Revision + 1
				_, err = q.UpdateWorkspaceStory(ctx, gen.UpdateWorkspaceStoryParams{ID: story.ID, Record: storyJSON(story), Revision: row.Revision})
				return err
			}
		}
		return nil
	})
}
