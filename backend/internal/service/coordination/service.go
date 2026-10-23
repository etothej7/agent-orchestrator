// Package coordination owns the single local coordinator and its repo managers.
package coordination

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
)

// Store supplies assignments, projects, sessions, and a shared lifecycle gate.
type Store interface {
	CoordinationMutex() *sync.RWMutex
	GetCoordination(context.Context) (domain.CoordinationConfig, error)
	SetCoordination(context.Context, domain.CoordinationConfig, time.Time) error
	GetProject(context.Context, string) (domain.ProjectRecord, bool, error)
	ListSessions(context.Context, domain.ProjectID) ([]domain.SessionRecord, error)
	CountUndeliveredManagerReports(context.Context) (int64, error)
	UpsertProject(context.Context, domain.ProjectRecord) error
}

// Service manages the local coordinator's scope.
type Service struct {
	store   Store
	dataDir string
}

// New constructs the coordinator settings service.
func New(store Store, dataDir string) *Service { return &Service{store: store, dataDir: dataDir} }

// CreateProject creates only AO-owned plain-directory coordination state. It
// never initializes Git in the user's development root or touches a repo.
func (s *Service) CreateProject(ctx context.Context, harness domain.AgentHarness) (domain.ProjectID, error) {
	if !s.store.CoordinationMutex().TryLock() {
		return "", apierr.Conflict("COORDINATION_BUSY", "An agent operation is in progress; retry coordinator setup shortly", nil)
	}
	defer s.store.CoordinationMutex().Unlock()
	if !harness.IsKnown() || harness == "" {
		return "", apierr.Invalid("UNKNOWN_HARNESS", "Choose a known coordinator agent", nil)
	}
	const id = "overall-coordinator"
	path := filepath.Join(s.dataDir, "coordination")
	existing, found, err := s.store.GetProject(ctx, id)
	if err != nil {
		return "", err
	}
	if found {
		if existing.Kind != domain.ProjectKindScratch || existing.Path != path {
			return "", apierr.Conflict("COORDINATOR_PROJECT_EXISTS", "The coordinator project name is already in use", nil)
		}
		if !existing.ArchivedAt.IsZero() {
			if err := os.MkdirAll(path, 0o700); err != nil {
				return "", err
			}
			existing.ArchivedAt = time.Time{}
			existing.Config.Orchestrator.Harness = harness
			if err := s.store.UpsertProject(ctx, existing); err != nil {
				return "", err
			}
		}
		return id, nil
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return "", err
	}
	err = s.store.UpsertProject(ctx, domain.ProjectRecord{
		ID: id, Path: path, DisplayName: "Overall coordinator", Kind: domain.ProjectKindScratch, RegisteredAt: time.Now().UTC(),
		Config: domain.ProjectConfig{Orchestrator: domain.RoleOverride{Harness: harness}},
	})
	return id, err
}

// Get returns the durable assignments.
func (s *Service) Get(ctx context.Context) (domain.CoordinationConfig, error) {
	return s.store.GetCoordination(ctx)
}

// Set changes routing without restarting or taking over existing agents.
func (s *Service) Set(ctx context.Context, cfg domain.CoordinationConfig) (domain.CoordinationConfig, error) {
	// ponytail: one shared gate for rare assignment changes; split per project
	// if unrelated operations cause frequent retries. Never queue a writer:
	// lifecycle restore paths can already hold their own workspace gates.
	if !s.store.CoordinationMutex().TryLock() {
		return cfg, apierr.Conflict("COORDINATION_BUSY", "An agent operation is in progress; retry saving assignments shortly", nil)
	}
	defer s.store.CoordinationMutex().Unlock()
	cfg.ManagedProjectIDs = slices.Clone(cfg.ManagedProjectIDs)
	if len(cfg.ManagedProjectIDs) > 1000 {
		return cfg, apierr.Invalid("INVALID_COORDINATION", "Choose at most 1000 repository projects", nil)
	}
	slices.Sort(cfg.ManagedProjectIDs)
	if cfg.CoordinatorProjectID == "" && len(cfg.ManagedProjectIDs) != 0 {
		return cfg, apierr.Invalid("COORDINATOR_REQUIRED", "Choose an overall coordinator first", nil)
	}
	ids := append([]domain.ProjectID{}, cfg.ManagedProjectIDs...)
	if cfg.CoordinatorProjectID != "" {
		ids = append(ids, cfg.CoordinatorProjectID)
	}
	seen := map[domain.ProjectID]bool{}
	for _, id := range ids {
		if id == "" || seen[id] {
			return cfg, apierr.Invalid("INVALID_COORDINATION", "Choose distinct projects; the coordinator cannot manage itself", nil)
		}
		seen[id] = true
		project, found, err := s.store.GetProject(ctx, string(id))
		if err != nil {
			return cfg, err
		}
		if !found || !project.ArchivedAt.IsZero() {
			return cfg, apierr.Invalid("INVALID_COORDINATION_PROJECT", "Choose existing, unarchived projects", nil)
		}
		if id == cfg.CoordinatorProjectID && project.Kind != domain.ProjectKindScratch {
			return cfg, apierr.Invalid("COORDINATOR_SCRATCH_REQUIRED", "The overall coordinator needs a separate Scratch project", nil)
		}
		if id != cfg.CoordinatorProjectID && project.Kind == domain.ProjectKindScratch {
			return cfg, apierr.Invalid("MANAGER_REPOSITORY_REQUIRED", "Repo managers need repository projects", nil)
		}
	}
	old, err := s.store.GetCoordination(ctx)
	if err != nil {
		return cfg, err
	}
	if old.CoordinatorProjectID == cfg.CoordinatorProjectID && slices.Equal(old.ManagedProjectIDs, cfg.ManagedProjectIDs) {
		return old, nil
	}
	// Existing prompts are immutable during a turn. Never silently change the
	// role of a live manager or change an active coordinator's scope.
	for _, id := range append(append(ids, old.ManagedProjectIDs...), old.CoordinatorProjectID) {
		if id == "" {
			continue
		}
		oldRole := role(old, id)
		newRole := role(cfg, id)
		if oldRole == newRole && oldRole != "coordinator" && old.CoordinatorProjectID == cfg.CoordinatorProjectID {
			continue
		}
		sessions, err := s.store.ListSessions(ctx, id)
		if err != nil {
			return cfg, err
		}
		for _, session := range sessions {
			if session.Kind == domain.KindOrchestrator && !session.IsTerminated {
				return cfg, apierr.Conflict("COORDINATION_SESSION_ACTIVE", "End the affected orchestrator session before changing its assignment", map[string]any{"sessionId": session.ID, "projectId": id})
			}
		}
	}
	{
		pending, err := s.store.CountUndeliveredManagerReports(ctx)
		if err != nil {
			return cfg, err
		}
		if pending != 0 {
			return cfg, apierr.Conflict("COORDINATION_REPORTS_PENDING", "Deliver pending manager reports before changing coordinator assignments", nil)
		}
	}
	if err := s.store.SetCoordination(ctx, cfg, time.Now().UTC()); err != nil {
		return cfg, err
	}
	return s.Get(ctx)
}

func role(cfg domain.CoordinationConfig, id domain.ProjectID) string {
	if id != "" && id == cfg.CoordinatorProjectID {
		return "coordinator"
	}
	if cfg.Manages(id) {
		return "manager"
	}
	return ""
}
