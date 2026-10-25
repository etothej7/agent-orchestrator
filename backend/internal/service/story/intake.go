package story

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/coordination"
)

// Intake binds a plan to one actual human message, not an operator's simulated
// creation transcript. No actors are spawned before every story scope validates.
type Intake struct {
	ID              string           `json:"id"`
	SourceMessageID string           `json:"sourceMessageId"`
	SessionID       domain.SessionID `json:"sessionId"`
	Epic            domain.Epic      `json:"epic"`
	Stories         []IntakeStory    `json:"stories"`
}

// IntakeStory declares one scoped deliverable in an atomic intake.
type IntakeStory struct {
	ID         string             `json:"id"`
	Title      string             `json:"title"`
	Scope      string             `json:"scope"`
	ProjectIDs []domain.ProjectID `json:"projectIds"`
}
type intakeStore interface {
	CreateStoryIntake(context.Context, string, string, domain.SessionID, string, domain.Epic, []domain.Story) error
}

// Intake adopts a human request into canonical epic and story records atomically.
func (s *Service) Intake(ctx context.Context, input Intake) error {
	store, ok := s.Store.(intakeStore)
	if !ok {
		return invalid("Durable intake is unavailable")
	}
	ctx, release := coordination.ReadLock(ctx, s.Store)
	defer release()
	if !validID(input.ID) || input.SourceMessageID == "" || len(input.SourceMessageID) > 200 || !validID(input.Epic.ID) || strings.TrimSpace(input.Epic.Title) == "" || len(input.Epic.Title) > 200 || len(input.Stories) == 0 || len(input.Stories) > 50 {
		return invalid("Intake needs a stable id, actual human message, epic and 1–50 stories")
	}
	cfg, err := s.Store.GetCoordination(ctx)
	if err != nil {
		return err
	}
	actor, found, err := s.Store.GetSession(ctx, input.SessionID)
	if err != nil {
		return err
	}
	if !found || actor.Kind != domain.KindOrchestrator || actor.ProjectID != cfg.CoordinatorProjectID || cfg.CoordinatorProjectID == "" || actor.IsTerminated || actor.Activity.State == domain.ActivityExited {
		return invalid("Only the active overall coordinator adopts an outcome")
	}
	stories := make([]domain.Story, 0, len(input.Stories))
	seen := map[string]bool{}
	for _, item := range input.Stories {
		if seen[item.ID] {
			return invalid("Story ids must be distinct")
		}
		seen[item.ID] = true
		row, err := s.prepare(ctx, domain.Story{ID: item.ID, EpicID: input.Epic.ID, Title: item.Title, Scope: item.Scope, ProjectIDs: item.ProjectIDs, Autonomous: true})
		if err != nil {
			return err
		}
		stories = append(stories, row)
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return err
	}
	return store.CreateStoryIntake(ctx, input.ID, input.SourceMessageID, input.SessionID, fmt.Sprintf("%x", sha256.Sum256(payload)), input.Epic, stories)
}
