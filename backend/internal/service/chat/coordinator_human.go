package chat

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/coordination"
)

type coordinatorHumanStore interface {
	GetCoordination(context.Context) (domain.CoordinationConfig, error)
	GetStory(context.Context, string) (domain.Story, error)
	CoordinatorHumanMessages(context.Context, string) (map[string]string, error)
	RecordCoordinatorHumanMessage(context.Context, domain.ConversationMessage, string, string, time.Time) (string, error)
}

// HumanDecision is a deliberate blocking question, never a status notification.
type HumanDecision struct {
	StoryID    string `json:"storyId,omitempty"`
	ID         string `json:"id"`
	Purpose    string `json:"purpose"`
	Question   string `json:"question"`
	WhyBlocked string `json:"whyBlocked"`
}

// RecordHumanDecision persists an explicit actionable question without dispatching a provider turn.
func (s *Service) RecordHumanDecision(ctx context.Context, id domain.SessionID, decision HumanDecision) (string, error) {
	store, ok := s.store.(coordinatorHumanStore)
	if !ok {
		return "", errors.New("coordinator decision store unavailable")
	}
	ctx, release := coordination.ReadLock(ctx, s.store)
	defer release()
	if strings.TrimSpace(decision.ID) == "" || len(decision.ID) > 128 || strings.TrimSpace(decision.Question) == "" || len(decision.Question) > 4000 || strings.TrimSpace(decision.WhyBlocked) == "" || len(decision.WhyBlocked) > 4000 {
		return "", apierr.Invalid("INVALID_HUMAN_DECISION", "A stable id, question and blocking reason are required", nil)
	}
	switch decision.Purpose {
	case "scope_question", "blocking_decision", "completion_decision":
	default:
		return "", apierr.Invalid("INVALID_HUMAN_DECISION", "Only scope questions and blocking decisions may contact the human", nil)
	}
	rec, err := s.requireChatSession(ctx, id)
	if err != nil {
		return "", err
	}
	cfg, err := store.GetCoordination(ctx)
	if err != nil {
		return "", err
	}
	if cfg.CoordinatorProjectID == "" || rec.ProjectID != cfg.CoordinatorProjectID || rec.Kind != domain.KindOrchestrator || rec.IsTerminated || rec.Activity.State == domain.ActivityExited {
		return "", apierr.Invalid("COORDINATOR_REQUIRED", "Route agent-resolvable issues through the responsible manager; only the active overall coordinator can ask the human", nil)
	}
	if decision.StoryID != "" {
		story, err := store.GetStory(ctx, decision.StoryID)
		if err != nil {
			return "", err
		}
		for _, project := range story.ProjectIDs {
			if !cfg.Manages(project) {
				return "", apierr.Invalid("DECISION_STORY_SCOPE", "Decision must belong to this coordinator's authorized repositories", nil)
			}
		}
	}
	conversation, err := s.store.ConversationForSession(ctx, id)
	if err != nil {
		return "", err
	}
	return store.RecordCoordinatorHumanMessage(ctx, domain.ConversationMessage{ID: "decision-" + decision.ID, ConversationID: conversation.ID, SenderSessionID: string(id), SenderProjectID: string(rec.ProjectID), SenderDisplayName: rec.DisplayName, Text: decision.Question + "\n\nWhy your input is needed: " + decision.WhyBlocked}, decision.Purpose, decision.StoryID, s.now())
}

// HumanSnapshot gates before serialization, including streaming messages. It
// does not mutate the durable/internal transcript or trust model prose markers.
func (s *Service) HumanSnapshot(ctx context.Context, in Snapshot) (Snapshot, error) {
	store, ok := s.store.(coordinatorHumanStore)
	if !ok {
		return in, nil
	}
	cfg, err := store.GetCoordination(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	if cfg.CoordinatorProjectID == "" || in.Conversation.ProjectID != cfg.CoordinatorProjectID || in.Conversation.ReviewID != "" {
		return in, nil
	}
	allowed, err := store.CoordinatorHumanMessages(ctx, in.Conversation.ID)
	if err != nil {
		return Snapshot{}, err
	}
	out := in
	out.Messages = nil
	out.Activities = nil
	out.Turns = nil
	out.BranchPoints = nil
	visibleTurns := map[string]bool{}
	for _, message := range in.Messages {
		if allowed[message.ID] != "" {
			message.Audience = "human"
			message.Purpose = allowed[message.ID]
			out.Messages = append(out.Messages, message)
			visibleTurns[message.TurnID] = true
		}
	}
	for _, activity := range in.Activities {
		if (activity.Kind != domain.ActivityKindApproval && activity.Kind != domain.ActivityKindUserInput) || activity.Status != domain.ActivityStatusPending {
			continue
		}
		activity.CommandOutput = ""
		activity.StreamedText = ""
		activity.ProviderItemID = ""
		activity.Detail = humanInputDetail(activity.Kind, activity.Detail)
		out.Activities = append(out.Activities, activity)
		visibleTurns[activity.TurnID] = true
	}
	for _, turn := range in.Turns {
		if !visibleTurns[turn.ID] {
			continue
		}
		turn.ProviderTurnID = ""
		turn.Diff = nil
		turn.Plan = nil
		if turn.ErrorMessage != "" {
			turn.ErrorMessage = "The coordinator could not complete this turn. Check its provider connection or account before retrying."
		}
		out.Turns = append(out.Turns, turn)
	}
	out.Conversation.MCPServers = nil
	out.Conversation.ThreadState = nil
	return out, nil
}

// Decode only the UI's actionable contract; never pass through provider fields.
func humanInputDetail(kind domain.ActivityKind, raw []byte) []byte {
	if kind == domain.ActivityKindUserInput {
		var input struct {
			InputMode     string         `json:"inputMode"`
			Message       string         `json:"message"`
			Schema        map[string]any `json:"schema,omitempty"`
			URL           string         `json:"url,omitempty"`
			ElicitationID string         `json:"elicitationId,omitempty"`
		}
		if json.Unmarshal(raw, &input) != nil {
			return nil
		}
		b, _ := json.Marshal(input)
		return b
	}
	var approval struct {
		Command   string `json:"command,omitempty"`
		Cwd       string `json:"cwd,omitempty"`
		Reason    string `json:"reason,omitempty"`
		Text      string `json:"text,omitempty"`
		Decisions []struct {
			ID    string `json:"id"`
			Label string `json:"label"`
			Kind  string `json:"kind,omitempty"`
		} `json:"decisions"`
	}
	if json.Unmarshal(raw, &approval) != nil {
		return nil
	}
	b, _ := json.Marshal(approval)
	return b
}

func (s *Service) isHumanCoordinator(ctx context.Context, project domain.ProjectID) (bool, error) {
	store, ok := s.store.(interface {
		GetCoordination(context.Context) (domain.CoordinationConfig, error)
		GetStory(context.Context, string) (domain.Story, error)
	})
	if !ok {
		return false, nil
	}
	cfg, err := store.GetCoordination(ctx)
	return cfg.CoordinatorProjectID != "" && cfg.CoordinatorProjectID == project, err
}

// RecordStartupFailure is invoked by lifecycle failure/recovery, not by an agent
// API. Store validation keeps it actionable and idempotent across restart.
func (s *Service) RecordStartupFailure(ctx context.Context, id domain.SessionID) error {
	store, ok := s.store.(interface {
		RecordCoordinatorStartupFailure(context.Context, domain.SessionID, time.Time) error
	})
	if !ok {
		return nil
	}
	ctx, release := coordination.ReadLock(ctx, s.store)
	defer release()
	return store.RecordCoordinatorStartupFailure(ctx, id, s.now())
}
