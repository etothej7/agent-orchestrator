package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// CoordinatorHumanMessages labels existing conversation messages; all ordinary
// model output stays internal by default in the overall coordinator's view.
func (s *Store) CoordinatorHumanMessages(ctx context.Context, conversationID string) (map[string]string, error) {
	rows, err := s.qr.ListCoordinatorHumanMessages(ctx, conversationID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		out[r.MessageID] = r.Purpose
	}
	return out, nil
}

// RecordCoordinatorHumanMessage atomically publishes one explicit decision and
// its audience marker. A lost response is retryable without duplicate dialogue.
func (s *Store) RecordCoordinatorHumanMessage(ctx context.Context, msg domain.ConversationMessage, purpose, storyID string, now time.Time) (string, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	err := s.inTx(ctx, "record coordinator decision", func(q *gen.Queries) error {
		old, err := q.GetCoordinatorHumanMessage(ctx, msg.ID)
		if err == nil {
			event, eventErr := q.GetWorkspaceEvent(ctx, msg.ID)
			if eventErr != nil && !errors.Is(eventErr, sql.ErrNoRows) {
				return eventErr
			}
			if event.StoryID != storyID {
				return apierr.Conflict("DECISION_KEY_REUSED", "Decision id belongs to different story scope", nil)
			}
			if old.ConversationID == msg.ConversationID && old.Text == msg.Text && old.Purpose == purpose && old.SenderSessionID == msg.SenderSessionID {
				return nil
			}
			return apierr.Conflict("DECISION_KEY_REUSED", "Decision id already identifies different content or sender", nil)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if storyID == "" {
			rows, err := q.ListWorkspaceStories(ctx)
			if err != nil {
				return err
			}
			for _, row := range rows {
				var story domain.Story
				if err := json.Unmarshal([]byte(row.Record), &story); err != nil {
					return err
				}
				if story.Autonomous && story.Status != "done" {
					return apierr.Invalid("DECISION_STORY_REQUIRED", "Name the story whose work must pause for this decision", nil)
				}
			}
		} else {
			row, err := q.GetWorkspaceStory(ctx, storyID)
			if err != nil {
				return err
			}
			var story domain.Story
			if err := json.Unmarshal([]byte(row.Record), &story); err != nil {
				return err
			}
			if !story.Autonomous || story.Status == "done" || story.Question != "" {
				return apierr.Conflict("DECISION_STORY_NOT_AVAILABLE", "Choose active autonomous work without an unresolved question", nil)
			}
			story.Question = msg.Text
			story.QuestionEventID = msg.ID
			story.QuestionRequesterSessionID = domain.SessionID(msg.SenderSessionID)
			story.Revision = row.Revision + 1
			if _, err := q.UpdateWorkspaceStory(ctx, gen.UpdateWorkspaceStoryParams{ID: story.ID, Record: storyJSON(story), Revision: row.Revision}); err != nil {
				return err
			}
			event := domain.StoryEvent{ID: msg.ID, StoryID: story.ID, Kind: "question", SenderSessionID: domain.SessionID(msg.SenderSessionID), Text: msg.Text, Reference: msg.ID, Result: "recorded", CreatedAt: now}
			if err := q.CreateWorkspaceEvent(ctx, gen.CreateWorkspaceEventParams{ID: event.ID, StoryID: story.ID, Record: storyJSON(event), Request: storyJSON(event)}); err != nil {
				return err
			}
		}
		sequence, err := q.NextConversationSequence(ctx, gen.NextConversationSequenceParams{ID: msg.ConversationID, UpdatedAt: now})
		if err != nil {
			return err
		}
		if err := q.InsertConversationMessage(ctx, gen.InsertConversationMessageParams{ID: msg.ID, ConversationID: msg.ConversationID, Sequence: sequence, Revision: 1, Role: domain.MessageRoleAssistant, Origin: domain.MessageOriginDaemon, Text: msg.Text, SenderSessionID: msg.SenderSessionID, SenderProjectID: msg.SenderProjectID, SenderDisplayName: msg.SenderDisplayName, CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		return q.InsertCoordinatorHumanMessage(ctx, gen.InsertCoordinatorHumanMessageParams{MessageID: msg.ID, Purpose: purpose})
	})
	return msg.ID, err
}
