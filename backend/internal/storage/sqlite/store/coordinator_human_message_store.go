package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
	return s.recordCoordinatorHumanMessage(ctx, msg, purpose, storyID, "", now)
}

// RecordCoordinatorStartupFailure exposes a daemon-owned blocker, not invented
// agent dialogue. The original queued request remains available to Retry.
func (s *Store) RecordCoordinatorStartupFailure(ctx context.Context, id domain.SessionID, now time.Time) error {
	rec, found, err := s.GetSession(ctx, id)
	if err != nil || !found {
		return err
	}
	cfg, err := s.GetCoordination(ctx)
	if err != nil {
		return err
	}
	if rec.Kind != domain.KindOrchestrator || rec.ProjectID != cfg.CoordinatorProjectID || rec.IsTerminated || rec.ProvisionState != domain.SessionProvisionFailed {
		return nil
	}
	conv, err := s.ConversationForSession(ctx, id)
	if errors.Is(err, domain.ErrNoConversation) {
		return nil
	}
	if err != nil {
		return err
	}
	msg := domain.ConversationMessage{
		ID:             fmt.Sprintf("startup-failed-%x", sha256.Sum256([]byte(string(id)+"\x00"+conv.ID))),
		ConversationID: conv.ID, SenderDisplayName: "AO",
		Text: "AO could not finish starting this coordinator. Your conversation is preserved. Check the startup error shown with your request, resolve the reported setup, provider or permission problem, then choose Retry. Automatic startup attempts have stopped; do not resend the request.",
	}
	_, err = s.recordCoordinatorHumanMessage(ctx, msg, "blocking_decision", "", id, now)
	return err
}

func (s *Store) recordCoordinatorHumanMessage(ctx context.Context, msg domain.ConversationMessage, purpose, storyID string, failedSession domain.SessionID, now time.Time) (string, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	err := s.inTx(ctx, "record coordinator decision", func(q *gen.Queries) error {
		if failedSession != "" {
			rec, err := q.GetSession(ctx, failedSession)
			if err != nil {
				return err
			}
			raw, err := q.GetCoordination(ctx)
			if err != nil {
				return err
			}
			var cfg domain.CoordinationConfig
			if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
				return err
			}
			if rec.Kind != domain.KindOrchestrator || rec.ProjectID == nil || *rec.ProjectID != cfg.CoordinatorProjectID || rec.IsTerminated || rec.ProvisionState != domain.SessionProvisionFailed {
				return nil
			}
			conversation, err := q.SelectConversationByID(ctx, msg.ConversationID)
			if err != nil {
				return err
			}
			owner := conversation.CurrentSessionID
			if owner == nil {
				owner = conversation.SessionID
			}
			if owner == nil || *owner != failedSession {
				return nil
			}
		}
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
		if storyID == "" && failedSession == "" {
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
		} else if storyID != "" {
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
