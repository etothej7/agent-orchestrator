package procidentity

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// maxRootsAge is how old the cached root list may be before it is no longer served: roots come
// from the daemon's own records and change only when sessions launch or stop, but a stale list
// must not outlive a failing refresh forever.
const maxRootsAge = time.Minute

// SessionRootsDeps are the daemon records the roots are read from.
type SessionRootsDeps struct {
	// List returns the sessions to register (terminated ones are skipped here).
	List func(ctx context.Context) ([]domain.SessionRecord, error)
	// PaneRoots returns the pane leader pids of a terminal session's runtime handle.
	PaneRoots func(ctx context.Context, handle string) ([]int, error)
	// ChatHostPID returns the live provider-host pid recorded for a chat session.
	ChatHostPID func(id domain.SessionID) (int, bool)
	// Procs supplies each root's start time from the kernel at registration.
	Procs ProcTable
	// TTL is how long a refresh is considered fresh.
	TTL time.Duration
	Now func() time.Time
}

// SessionRoots is the RootSource backed by the daemon's records: for every live session the
// process(es) the DAEMON started for it (a tmux pane's leader, a chat provider host), read from
// daemon records and never from a caller, each with its kernel start time. The list is cached; a
// stale cache is answered at once and refreshed in the background, so no request waits on a
// listing of sessions and tmux panes (except the very first one).
type SessionRoots struct {
	deps SessionRootsDeps

	mu         sync.Mutex
	cached     []Root
	cachedAt   time.Time
	have       bool
	refreshing bool
	idle       chan struct{}
}

// NewSessionRoots builds a SessionRoots.
func NewSessionRoots(deps SessionRootsDeps) *SessionRoots {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.TTL <= 0 {
		deps.TTL = 5 * time.Second
	}
	return &SessionRoots{deps: deps}
}

// Roots returns the current roots.
func (s *SessionRoots) Roots(ctx context.Context) ([]Root, error) {
	s.mu.Lock()
	now := s.deps.Now()
	if !s.have {
		s.mu.Unlock()
		return s.refreshNow(ctx) // the first call has nothing to serve: it waits
	}
	age := now.Sub(s.cachedAt)
	cached := s.cached
	if age < s.deps.TTL {
		s.mu.Unlock()
		return cached, nil
	}
	if !s.refreshing {
		s.refreshing = true
		s.idle = make(chan struct{})
		go s.refreshBackground(context.WithoutCancel(ctx))
	}
	s.mu.Unlock()
	if age > maxRootsAge {
		return nil, errors.New("session roots are too old and could not be refreshed")
	}
	return cached, nil
}

func (s *SessionRoots) refreshNow(ctx context.Context) ([]Root, error) {
	roots, err := s.build(ctx)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.cached, s.cachedAt, s.have = roots, s.deps.Now(), true
	s.mu.Unlock()
	return roots, nil
}

func (s *SessionRoots) refreshBackground(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	roots, err := s.build(ctx)
	s.mu.Lock()
	if err == nil {
		s.cached, s.cachedAt, s.have = roots, s.deps.Now(), true
	}
	s.refreshing = false
	idle := s.idle
	s.mu.Unlock()
	close(idle)
}

// waitIdle lets tests wait for a background refresh.
func (s *SessionRoots) waitIdle() {
	s.mu.Lock()
	idle := s.idle
	refreshing := s.refreshing
	s.mu.Unlock()
	if refreshing && idle != nil {
		<-idle
	}
}

func (s *SessionRoots) build(ctx context.Context) ([]Root, error) {
	sessions, err := s.deps.List(ctx)
	if err != nil {
		return nil, err
	}
	var roots []Root
	add := func(id domain.SessionID, pid int) {
		if pid <= 1 {
			return
		}
		info, err := s.deps.Procs.Info(pid)
		if err != nil {
			return // already gone
		}
		roots = append(roots, Root{Session: id, PID: pid, Start: info.Start})
	}
	for _, rec := range sessions {
		if rec.IsTerminated {
			continue
		}
		if rec.Mode == domain.SessionModeChat {
			if s.deps.ChatHostPID != nil {
				if pid, ok := s.deps.ChatHostPID(rec.ID); ok {
					add(rec.ID, pid)
				}
			}
			continue
		}
		if rec.Metadata.RuntimeHandleID == "" || s.deps.PaneRoots == nil {
			continue
		}
		pids, err := s.deps.PaneRoots(ctx, rec.Metadata.RuntimeHandleID)
		if err != nil {
			continue // a probe failure registers nothing for this session (fail closed)
		}
		for _, pid := range pids {
			add(rec.ID, pid)
		}
	}
	return roots, nil
}
