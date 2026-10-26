package chat

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/coordination"
)

type coordinatorHumanStore interface {
	GetCoordination(context.Context) (domain.CoordinationConfig, error)
	GetStory(context.Context, string) (domain.Story, error)
	CoordinatorHumanMessages(context.Context, string) (map[string]string, error)
	RecordCoordinatorHumanMessage(context.Context, domain.ConversationMessage, string, string, time.Time) (string, error)
}

// HumanNotifier raises the human-facing notification for an explicit coordinator message.
type HumanNotifier interface {
	Notify(context.Context, ports.NotificationIntent) error
}

// HumanDecision is a deliberate message to the human: a blocking question or
// decision, or a bounded notice of a verified result or stopped work. It is
// never a status notification.
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
	notice := decision.Purpose == "verified_result" || decision.Purpose == "stopped_work"
	if strings.TrimSpace(decision.ID) == "" || len(decision.ID) > 128 || strings.TrimSpace(decision.Question) == "" || len(decision.Question) > 4000 || len(decision.WhyBlocked) > 4000 || (!notice && strings.TrimSpace(decision.WhyBlocked) == "") {
		return "", apierr.Invalid("INVALID_HUMAN_DECISION", "A stable id, question and blocking reason are required", nil)
	}
	switch decision.Purpose {
	case "scope_question", "blocking_decision", "completion_decision", "verified_result", "stopped_work":
	default:
		return "", apierr.Invalid("INVALID_HUMAN_DECISION", "Only scope questions, blocking decisions, verified results and stopped work may contact the human", nil)
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
	text := decision.Question
	if !notice {
		text += "\n\nWhy your input is needed: " + decision.WhyBlocked
	}
	messageID, err := store.RecordCoordinatorHumanMessage(ctx, domain.ConversationMessage{ID: "decision-" + decision.ID, ConversationID: conversation.ID, SenderSessionID: string(id), SenderProjectID: string(rec.ProjectID), SenderDisplayName: rec.DisplayName, Text: text}, decision.Purpose, decision.StoryID, s.now())
	if err != nil {
		return "", err
	}
	// The notification is keyed by the message, so a retry after a lost response
	// completes a missing one without raising a second.
	if s.notifier != nil {
		if err := s.notifier.Notify(ctx, humanNotification(id, rec.ProjectID, rec.DisplayName, messageID, decision, s.now())); err != nil {
			return "", err
		}
	}
	return messageID, nil
}

func humanNotification(id domain.SessionID, project domain.ProjectID, name, messageID string, decision HumanDecision, at time.Time) ports.NotificationIntent {
	title := "Your coordinator needs a decision"
	switch decision.Purpose {
	case "verified_result":
		title = "Verified result from your coordinator"
	case "stopped_work":
		title = "Work has stopped and needs you"
	}
	body := strings.TrimSpace(decision.Question)
	if runes := []rune(body); len(runes) > 280 {
		body = string(runes[:280]) + "…"
	}
	return ports.NotificationIntent{Type: domain.NotificationCoordinatorMessage, SessionID: id, ProjectID: project, CreatedAt: at, SourceKey: "coordinator-message:" + messageID, SessionDisplayName: name, Title: title, Body: body, Purpose: decision.Purpose}
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
		if text, ok := humanSteerText(activity); ok {
			// Ed's own guidance into a running turn: show his words, nothing else.
			activity.CommandOutput = ""
			activity.StreamedText = ""
			activity.ProviderItemID = ""
			activity.Detail, _ = json.Marshal(map[string]string{"event": "steer", "text": text, "origin": "human"})
			out.Activities = append(out.Activities, activity)
			visibleTurns[activity.TurnID] = true
			continue
		}
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

// TagSnapshotAudience annotates the unfiltered snapshot of the overall
// coordinator conversation: human is exactly what HumanSnapshot would return for
// the item, internal is everything else. Other conversations come back
// untouched, so audience=all is silently ignored for them.
func (s *Service) TagSnapshotAudience(ctx context.Context, in Snapshot) (Snapshot, error) {
	visible, err := s.HumanSnapshot(ctx, in)
	if err != nil {
		return Snapshot{}, err
	}
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
	purposes := make(map[string]string, len(visible.Messages))
	for _, message := range visible.Messages {
		purposes[message.ID] = message.Purpose
	}
	activities := make(map[string]bool, len(visible.Activities))
	for _, activity := range visible.Activities {
		activities[activity.ID] = true
	}
	turns := make(map[string]bool, len(visible.Turns))
	for _, turn := range visible.Turns {
		turns[turn.ID] = true
	}
	out := in
	out.Messages = make([]domain.ConversationMessage, len(in.Messages))
	for i, message := range in.Messages {
		if purpose, human := purposes[message.ID]; human {
			message.Audience, message.Purpose = "human", purpose
		} else {
			message.Audience = "internal"
		}
		out.Messages[i] = message
	}
	out.Activities = make([]domain.ConversationActivity, len(in.Activities))
	for i, activity := range in.Activities {
		activity.Audience = tagAudience(activities[activity.ID])
		out.Activities[i] = activity
	}
	out.Turns = make([]domain.ConversationTurn, len(in.Turns))
	for i, turn := range in.Turns {
		turn.Audience = tagAudience(turns[turn.ID])
		out.Turns[i] = turn
	}
	return out, nil
}

func tagAudience(human bool) string {
	if human {
		return "human"
	}
	return "internal"
}

// humanSteerText recognizes guidance Ed typed into a running turn: a steer the
// human sent directly (/conversation/steer or a promoted queued message), not
// one an agent relayed. Its reply is visible like any answer to Ed.
func humanSteerText(activity domain.ConversationActivity) (string, bool) {
	if activity.Kind != domain.ActivityKindSystem {
		return "", false
	}
	var detail struct {
		Event           string `json:"event"`
		Text            string `json:"text"`
		Origin          string `json:"origin"`
		SenderSessionID string `json:"senderSessionId"`
	}
	if json.Unmarshal(activity.Detail, &detail) != nil {
		return "", false
	}
	if detail.Event != "steer" || detail.Origin != string(domain.MessageOriginHuman) || detail.SenderSessionID != "" {
		return "", false
	}
	return detail.Text, true
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
