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

// ErrWorkRunning is what a Manager returns when the session is idle but its agent has background
// work running (a test run, a dev server, a build). Relaunching would kill that work, so the
// session is left alone and tried again later; it is not a failure either.
var ErrWorkRunning = errors.New("session has background work running")

// ErrResumeFailed is what a Manager returns when the session WAS stopped for the relaunch but could
// not be started again. The session is down: unlike a skip, this must be followed up.
var ErrResumeFailed = errors.New("session was stopped but could not be started again")

// Manager relaunches one session with its conversation resumed, only if it is idle.
type Manager interface {
	// RelaunchForIdentity reports whether the session was actually relaunched: false, nil is a
	// session that needed nothing (ended, asleep).
	RelaunchForIdentity(ctx context.Context, id domain.SessionID) (bool, error)
	// ResumeStopped starts a session a relaunch stopped and could not start again. It reports
	// false, nil when the session no longer needs it (it came back or ended).
	ResumeStopped(ctx context.Context, id domain.SessionID) (bool, error)
}

// Stopped is how the service makes sure a session it stopped never stays down unnoticed.
type Stopped interface {
	// RecordStopped durably records that the session is down and should be brought back, so the
	// auto-resume pass of the next daemon start revives it even if the daemon restarts first.
	RecordStopped(ctx context.Context, id domain.SessionID) error
	// NotifyGaveUp tells the person once that the session is stopped and the service has stopped
	// trying to bring it back (the standard "work has stopped" notice).
	NotifyGaveUp(ctx context.Context, id domain.SessionID, reason string) error
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
	// MaxWorkWait is how long a session with background work running is waited for before the
	// service logs that it is still staying on its old identity (default 2h). It never kills the
	// work and keeps asking; the session is relaunched the first time it is quiet.
	MaxWorkWait time.Duration
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
	if c.MaxWorkWait <= 0 {
		c.MaxWorkWait = 2 * time.Hour
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return c
}

// Service is the rolling re-proof.
type Service struct {
	mgr     Manager
	store   Store
	stopped Stopped
	log     *slog.Logger
	cfg     Config

	mu          sync.Mutex
	started     time.Time
	failures    map[domain.SessionID]int
	nextTry     map[domain.SessionID]time.Time
	gaveUp      map[domain.SessionID]bool
	lastProject map[domain.ProjectID]time.Time
	priority    map[domain.SessionID]time.Time
	// down are sessions a relaunch stopped and could not start again, retried until they are back
	// or the service gives up (and says so).
	down map[domain.SessionID]*downState
	// workSince is when a session was first seen with background work running; workWarned is set
	// once the wait went past MaxWorkWait and was logged.
	workSince  map[domain.SessionID]time.Time
	workWarned map[domain.SessionID]bool
}

type downState struct {
	attempts int
	nextTry  time.Time
}

// New builds a Service.
func New(mgr Manager, store Store, stopped Stopped, log *slog.Logger, cfg Config) *Service {
	cfg = cfg.withDefaults()
	return &Service{
		mgr: mgr, store: store, stopped: stopped, log: log, cfg: cfg, started: cfg.Now(),
		down: map[domain.SessionID]*downState{}, workSince: map[domain.SessionID]time.Time{}, workWarned: map[domain.SessionID]bool{},
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
	if s.retryDown(ctx, now) {
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
	relaunched, err := s.mgr.RelaunchForIdentity(ctx, rec.ID)
	switch {
	case errors.Is(err, ErrBusy):
		return false
	case errors.Is(err, ErrWorkRunning):
		s.waitForWork(rec, now)
		return false
	case err == nil && !relaunched:
		return false // nothing to do for this session (ended or asleep): not a relaunch
	case err == nil:
		s.mu.Lock()
		s.lastProject[rec.ProjectID] = now
		delete(s.priority, rec.ID)
		delete(s.failures, rec.ID)
		delete(s.workSince, rec.ID)
		delete(s.workWarned, rec.ID)
		s.mu.Unlock()
		s.log.Info("re-proof: relaunched a session to give it an identity proof", "session", rec.ID, "project", rec.ProjectID, "kind", string(rec.Kind))
		return true
	case errors.Is(err, ErrResumeFailed):
		s.sessionIsDown(ctx, rec, now, err)
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

// waitForWork notes that a session is idle but has background work running. The first sighting is
// logged at debug; once the wait passes MaxWorkWait it is logged ONCE at info (the session stays on
// its old identity and keeps being asked about). The work is never killed.
func (s *Service) waitForWork(rec domain.SessionRecord, now time.Time) {
	s.mu.Lock()
	since, seen := s.workSince[rec.ID]
	if !seen {
		s.workSince[rec.ID] = now
		since = now
	}
	warn := now.Sub(since) >= s.cfg.MaxWorkWait && !s.workWarned[rec.ID]
	if warn {
		s.workWarned[rec.ID] = true
	}
	s.mu.Unlock()
	switch {
	case warn:
		s.log.Info("re-proof: a session still has background work running; it stays on its old identity until that ends (nothing is killed)",
			"session", rec.ID, "project", rec.ProjectID, "waited", now.Sub(since).Round(time.Minute).String())
	case !seen:
		s.log.Debug("re-proof: a session has background work running; trying again later", "session", rec.ID)
	}
}

// sessionIsDown handles a relaunch that stopped the session and could not start it again. The
// failure counts as the first attempt, the intent to bring it back is made durable, and the
// session is retried by retryDown until it is back or the attempts run out.
func (s *Service) sessionIsDown(ctx context.Context, rec domain.SessionRecord, now time.Time, cause error) {
	s.mu.Lock()
	s.lastProject[rec.ProjectID] = now
	s.down[rec.ID] = &downState{attempts: 1, nextTry: now.Add(s.cfg.BackoffBase)}
	s.mu.Unlock()
	s.log.Warn("re-proof: a session was stopped for its relaunch and could not be started again; retrying", "session", rec.ID, "error", cause)
	if s.stopped != nil {
		if err := s.stopped.RecordStopped(ctx, rec.ID); err != nil {
			s.log.Warn("re-proof: could not record the intent to resume a stopped session", "session", rec.ID, "error", err)
		}
	}
}

// retryDown retries ONE session that a relaunch stopped and could not start again, when its
// backoff is up. It returns true when it used this tick. After MaxAttempts failed attempts it gives
// up and posts the standard "work has stopped" notice; the recorded resume intent still stands for
// the next daemon start.
func (s *Service) retryDown(ctx context.Context, now time.Time) bool {
	s.mu.Lock()
	var id domain.SessionID
	for cand, st := range s.down {
		if !now.Before(st.nextTry) && (id == "" || cand < id) {
			id = cand
		}
	}
	s.mu.Unlock()
	if id == "" {
		return false
	}
	resumed, err := s.mgr.ResumeStopped(ctx, id)
	s.mu.Lock()
	st := s.down[id]
	if err == nil {
		delete(s.down, id)
		s.mu.Unlock()
		if resumed {
			s.log.Info("re-proof: a session stopped for its relaunch is running again", "session", id)
		}
		return true
	}
	st.attempts++
	giveUp := st.attempts >= s.cfg.MaxAttempts
	if giveUp {
		delete(s.down, id)
		s.gaveUp[id] = true
	} else {
		st.nextTry = now.Add(s.cfg.BackoffBase << (st.attempts - 1))
	}
	attempts := st.attempts
	s.mu.Unlock()
	s.log.Warn("re-proof: starting a stopped session again failed", "session", id, "attempt", attempts, "gave_up", giveUp, "error", err)
	if giveUp && s.stopped != nil {
		if nerr := s.stopped.NotifyGaveUp(ctx, id, "its identity relaunch stopped it and it could not be started again"); nerr != nil {
			s.log.Warn("re-proof: could not post the notice that a session is stopped", "session", id, "error", nerr)
		}
	}
	return true
}
