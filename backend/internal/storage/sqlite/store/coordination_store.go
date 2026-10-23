package store

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// CoordinationMutex serializes assignment changes against lifecycle operations.
func (s *Store) CoordinationMutex() *sync.RWMutex { return &s.coordinationMu }

// GetCoordination reads the saved coordinator assignments.
func (s *Store) GetCoordination(ctx context.Context) (domain.CoordinationConfig, error) {
	var cfg domain.CoordinationConfig
	raw, err := s.qr.GetCoordination(ctx)
	if err != nil {
		return cfg, err
	}
	err = json.Unmarshal([]byte(raw), &cfg)
	return cfg, err
}

// SetCoordination persists assignments after the coordination service's checks.
func (s *Store) SetCoordination(ctx context.Context, cfg domain.CoordinationConfig, now time.Time) error {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.qw.SetCoordination(ctx, gen.SetCoordinationParams{Coordination: string(raw), UpdatedAt: now})
}

// CountUndeliveredManagerReports includes pending and claimed upward reports.
func (s *Store) CountUndeliveredManagerReports(ctx context.Context) (int64, error) {
	return s.qr.CountUndeliveredManagerReports(ctx)
}
