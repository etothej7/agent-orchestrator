// Package reproof gives sessions that were already running when identity proofs arrived a proof,
// by relaunching each one — with its conversation resumed — at a moment it is idle (ED8 rollout).
//
// Sessions keep the environment they were launched with, so a session started before the upgrade
// has no proof, and only a new launch can give it one. This service does that automatically and
// carefully: one session per tick; the overall coordinator first, then project managers, then
// workers; never a session that is not idle, never one whose auto-resume policy is "never", never
// a terminated or hibernated one (a wake issues its proof); a failing relaunch backs off and gives
// up after a few attempts. A session the daemon refused for lack of a proof can be moved to the
// front of the queue (Prioritize). The relaunch itself, and the idle check under the session's
// lock, are the session manager's.
package reproof

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// ErrBusy is what a Manager returns when the session turned out not to be idle (a turn started, an
// approval is pending). It is not a failure: the session is simply tried again later.
var ErrBusy = errors.New("session is busy")

// Manager relaunches one session with its conversation resumed, only if it is idle.
type Manager interface {
	RelaunchForIdentity(ctx context.Context, id domain.SessionID) error
}

// Store is the daemon's records the service reads.
type Store interface {
	ListAllSessions(ctx context.Context) ([]domain.SessionRecord, error)
	SessionsWithProof(ctx context.Context, ids []domain.SessionID) (map[domain.SessionID]bool, error)
	AutoResumeAllowed(ctx context.Context, id domain.SessionID) bool
	GetCoordination(ctx context.Context) (domain.CoordinationConfig, error)
}

// Config tunes the service. Zero values take the defaults; a negative StartupDelay disables it.
type Config struct {
	// Every is the tick interval: at most one session is relaunched per tick (default 30s).
	Every time.Duration
	// StartupDelay lets the fleet settle after the daemon starts (default 3m).
	StartupDelay time.Duration
	// ProjectCooldown keeps two sessions of one project from being relaunched back to back
	// (default 5m).
	ProjectCooldown time.Duration
	// MaxAttempts bounds retries of a failing relaunch (default 3); BackoffBase is the first
	// delay, doubling (default 2m).
	MaxAttempts int
	BackoffBase time.Duration
	Now         func() time.Time
}

func (c Config) withDefaults() Config {
	if c.Every <= 0 {
		c.Every = 30 * time.Second
	}
	switch {
	case c.StartupDelay == 0:
		c.StartupDelay = 3 * time.Minute
	case c.StartupDelay < 0:
		c.StartupDelay = 0
	}
	if c.ProjectCooldown <= 0 {
		c.ProjectCooldown = 5 * time.Minute
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 3
	}
	if c.BackoffBase <= 0 {
		c.BackoffBase = 2 * time.Minute
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return c
}

// Service is the rolling re-proof.
type Service struct {
	mgr   Manager
	store Store
	log   *slog.Logger
	cfg   Config

	mu          sync.Mutex
	started     time.Time
	failures    map[domain.SessionID]int
	nextTry     map[domain.SessionID]time.Time
	gaveUp      map[domain.SessionID]bool
	lastProject map[domain.ProjectID]time.Time
	priority    map[domain.SessionID]time.Time
}

// New builds a Service.
func New(mgr Manager, store Store, log *slog.Logger, cfg Config) *Service {
	cfg = cfg.withDefaults()
	return &Service{
		mgr: mgr, store: store, log: log, cfg: cfg, started: cfg.Now(),
		failures: map[domain.SessionID]int{}, nextTry: map[domain.SessionID]time.Time{}, gaveUp: map[domain.SessionID]bool{},
		lastProject: map[domain.ProjectID]time.Time{}, priority: map[domain.SessionID]time.Time{},
	}
}

// Run ticks until ctx ends.
func (s *Service) Run(ctx context.Context) {
	t := time.NewTicker(s.cfg.Every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Tick(ctx)
		}
	}
}

// GaveUp reports whether the service stopped trying to relaunch the session.
func (s *Service) GaveUp(id domain.SessionID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gaveUp[id]
}

// Prioritize moves a live session that has no proof to the front of the queue. It returns true
// when the session will be relaunched (live, un-proven, auto-resume allowed), false for an unknown,
// terminated, proven or auto-resume-never session. It never relaunches anything itself.
func (s *Service) Prioritize(ctx context.Context, id domain.SessionID) bool {
	if id == "" {
		return false
	}
	recs, err := s.store.ListAllSessions(ctx)
	if err != nil {
		return false
	}
	for _, rec := range recs {
		if rec.ID != id {
			continue
		}
		if !s.needsProof(ctx, rec) {
			return false
		}
		proven, err := s.store.SessionsWithProof(ctx, []domain.SessionID{id})
		if err != nil || proven[id] {
			return false
		}
		s.mu.Lock()
		if _, ok := s.priority[id]; !ok {
			s.priority[id] = s.cfg.Now()
		}
		s.mu.Unlock()
		return true
	}
	return false
}

// needsProof is the part of the rules that does not depend on the proof table: a live, awake
// session whose auto-resume policy allows a relaunch.
func (s *Service) needsProof(ctx context.Context, rec domain.SessionRecord) bool {
	return !rec.IsTerminated && rec.HibernatedAt == nil && s.store.AutoResumeAllowed(ctx, rec.ID)
}

// Tick relaunches at most one session.
func (s *Service) Tick(ctx context.Context) {
	now := s.cfg.Now()
	s.mu.Lock()
	settled := now.Sub(s.started) >= s.cfg.StartupDelay
	s.mu.Unlock()
	if !settled {
		return
	}
	recs, err := s.store.ListAllSessions(ctx)
	if err != nil {
		s.log.Warn("re-proof: could not list sessions", "error", err)
		return
	}
	var live []domain.SessionRecord
	var ids []domain.SessionID
	for _, rec := range recs {
		if s.needsProof(ctx, rec) {
			live = append(live, rec)
			ids = append(ids, rec.ID)
		}
	}
	if len(ids) == 0 {
		return
	}
	proven, err := s.store.SessionsWithProof(ctx, ids)
	if err != nil {
		s.log.Warn("re-proof: could not read proofs", "error", err)
		return
	}
	cfg, err := s.store.GetCoordination(ctx)
	if err != nil {
		s.log.Warn("re-proof: could not read coordination", "error", err)
		return
	}
	var candidates []domain.SessionRecord
	for _, rec := range live {
		if !proven[rec.ID] {
			candidates = append(candidates, rec)
		}
	}
	s.mu.Lock()
	for id := range s.priority { // a prioritized session that is no longer a candidate is forgotten
		found := false
		for _, rec := range candidates {
			if rec.ID == id {
				found = true
			}
		}
		if !found {
			delete(s.priority, id)
		}
	}
	prio := map[domain.SessionID]time.Time{}
	for id, at := range s.priority {
		prio[id] = at
	}
	s.mu.Unlock()
	tier := func(rec domain.SessionRecord) int {
		switch {
		case rec.Kind == domain.KindOrchestrator && cfg.CoordinatorProjectID != "" && rec.ProjectID == cfg.CoordinatorProjectID:
			return 0
		case rec.Kind == domain.KindOrchestrator:
			return 1
		}
		return 2
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		pi, iok := prio[candidates[i].ID]
		pj, jok := prio[candidates[j].ID]
		if iok != jok {
			return iok
		}
		if iok && jok && !pi.Equal(pj) {
			return pi.Before(pj)
		}
		ti, tj := tier(candidates[i]), tier(candidates[j])
		if ti != tj {
			return ti < tj
		}
		return candidates[i].CreatedAt.Before(candidates[j].CreatedAt)
	})
	for _, rec := range candidates {
		if s.attempt(ctx, rec, now) {
			return
		}
	}
}

// attempt tries one candidate; it returns true when it used this tick's relaunch (a success or a
// failure), false when the candidate was skipped.
func (s *Service) attempt(ctx context.Context, rec domain.SessionRecord, now time.Time) bool {
	s.mu.Lock()
	if s.gaveUp[rec.ID] || now.Before(s.nextTry[rec.ID]) {
		s.mu.Unlock()
		return false
	}
	if last, ok := s.lastProject[rec.ProjectID]; ok && now.Sub(last) < s.cfg.ProjectCooldown {
		s.mu.Unlock()
		return false
	}
	s.mu.Unlock()
	if rec.Activity.State != domain.ActivityIdle {
		return false // never mid-turn: the manager checks again under the session's lock
	}
	err := s.mgr.RelaunchForIdentity(ctx, rec.ID)
	switch {
	case errors.Is(err, ErrBusy):
		return false
	case err == nil:
		s.mu.Lock()
		s.lastProject[rec.ProjectID] = now
		delete(s.priority, rec.ID)
		delete(s.failures, rec.ID)
		s.mu.Unlock()
		s.log.Info("re-proof: relaunched a session to give it an identity proof", "session", rec.ID, "project", rec.ProjectID, "kind", string(rec.Kind))
		return true
	}
	s.mu.Lock()
	s.failures[rec.ID]++
	n := s.failures[rec.ID]
	s.lastProject[rec.ProjectID] = now
	if n >= s.cfg.MaxAttempts {
		s.gaveUp[rec.ID] = true
	} else {
		s.nextTry[rec.ID] = now.Add(s.cfg.BackoffBase << (n - 1))
	}
	s.mu.Unlock()
	s.log.Warn("re-proof: relaunch failed", "session", rec.ID, "attempt", n, "gave_up", n >= s.cfg.MaxAttempts, "error", err)
	return true
}
