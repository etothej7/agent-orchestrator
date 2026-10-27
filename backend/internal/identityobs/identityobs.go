// Package identityobs counts and describes the calls that reach the daemon without an identity
// (ED8 observe mode): per route, per claimed session, and (best effort) per calling process, so
// the tools that still call without a proof can be found and fixed before enforcement flips.
//
// Everything here is a diagnostic sink. Recording returns nothing and never waits; the pid and
// process name come from a best-effort lookup that is rate limited, timed out and bounded, may
// fail at any time (reported as "unavailable"), and is never an input to who a caller is or what
// it may do.
package identityobs

import (
	"sort"
	"sync"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/procidentity"
)

// Unavailable is the process name reported when the caller could not be looked up.
const Unavailable = "unavailable"

// PeerLookup maps a loopback connection to the pid owning the client end.
type PeerLookup interface {
	OwnerPID(clientPort, serverPort int) (int, error)
}

// ProcTable describes a process.
type ProcTable interface {
	Info(pid int) (procidentity.Proc, error)
}

// Config bounds a Recorder. Zero values take the defaults.
type Config struct {
	// LookupTimeout is how long a lookup may take before it is reported unavailable (default 250ms).
	LookupTimeout time.Duration
	// MaxLookupsPerSecond and Burst are the lookup rate limit (default 20/s, burst 20).
	MaxLookupsPerSecond float64
	Burst               int
	// MaxKeys caps each of the route, session and process tables (default 1024); calls past the
	// cap are still counted in the totals.
	MaxKeys int
	// MaxInFlight caps concurrent lookups (default 4).
	MaxInFlight int
	Now         func() time.Time
	// OnLookup, when set, is told the outcome of each lookup (pid 0 and name "unavailable" when it
	// failed), for logging. It runs on the lookup goroutine, never on a request.
	OnLookup func(c Call, pid int, name string)
}

// Call is one request from a caller that proved nothing.
type Call struct {
	Method, Route, Class string
	// Claimed is the session the caller said it was (never an identity).
	Claimed domain.SessionID
	Allowed bool
	Reason  string
	// ClientPort and ServerPort identify the loopback connection (0 when not loopback).
	ClientPort, ServerPort int
}

// Recorder is the in-memory observation store.
type Recorder struct {
	peer  PeerLookup
	procs ProcTable
	cfg   Config

	mu       sync.Mutex
	since    time.Time
	total    int
	allowed  int
	refused  int
	overflow int
	dropped  int
	routes   map[string]*routeStat
	sessions map[domain.SessionID]*sessionStat
	procsBy  map[string]*procStat
	tokens   float64
	refilled time.Time

	inflight chan struct{}
	wg       sync.WaitGroup
}

type routeStat struct {
	method, route, class    string
	calls, allowed, refused int
	last                    time.Time
}

type sessionStat struct {
	calls int
	last  time.Time
}

type procStat struct {
	calls   int
	lastPID int
	last    time.Time
}

// New builds a Recorder. peer and procs may be nil (every process is then "unavailable").
func New(peer PeerLookup, procs ProcTable, cfg Config) *Recorder {
	if cfg.LookupTimeout <= 0 {
		cfg.LookupTimeout = 250 * time.Millisecond
	}
	if cfg.MaxLookupsPerSecond <= 0 {
		cfg.MaxLookupsPerSecond = 20
	}
	if cfg.Burst <= 0 {
		cfg.Burst = 20
	}
	if cfg.MaxKeys <= 0 {
		cfg.MaxKeys = 1024
	}
	if cfg.MaxInFlight <= 0 {
		cfg.MaxInFlight = 4
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	now := cfg.Now()
	return &Recorder{
		peer: peer, procs: procs, cfg: cfg, since: now,
		routes: map[string]*routeStat{}, sessions: map[domain.SessionID]*sessionStat{}, procsBy: map[string]*procStat{},
		tokens: float64(cfg.Burst), refilled: now,
		inflight: make(chan struct{}, cfg.MaxInFlight),
	}
}

// Record counts one anonymous call and, best effort and off the caller's path, names its process.
func (r *Recorder) Record(c Call) {
	now := r.cfg.Now()
	r.mu.Lock()
	r.total++
	if c.Allowed {
		r.allowed++
	} else {
		r.refused++
	}
	key := c.Method + " " + c.Route
	rs := r.routes[key]
	if rs == nil {
		if len(r.routes) >= r.cfg.MaxKeys {
			r.overflow++
		} else {
			rs = &routeStat{method: c.Method, route: c.Route, class: c.Class}
			r.routes[key] = rs
		}
	}
	if rs != nil {
		rs.calls++
		rs.last = now
		if c.Allowed {
			rs.allowed++
		} else {
			rs.refused++
		}
	}
	if c.Claimed != "" {
		ss := r.sessions[c.Claimed]
		if ss == nil && len(r.sessions) < r.cfg.MaxKeys {
			ss = &sessionStat{}
			r.sessions[c.Claimed] = ss
		}
		if ss != nil {
			ss.calls++
			ss.last = now
		}
	}
	lookup := r.takeToken(now)
	if !lookup {
		r.dropped++
	}
	r.mu.Unlock()

	if !lookup {
		return
	}
	select {
	case r.inflight <- struct{}{}:
		r.wg.Add(1)
		go r.lookup(c, now)
	default:
		r.mu.Lock()
		r.dropped++
		r.mu.Unlock()
	}
}

// takeToken applies the lookup rate limit; the caller holds r.mu.
func (r *Recorder) takeToken(now time.Time) bool {
	if elapsed := now.Sub(r.refilled).Seconds(); elapsed > 0 {
		r.tokens += elapsed * r.cfg.MaxLookupsPerSecond
		if r.tokens > float64(r.cfg.Burst) {
			r.tokens = float64(r.cfg.Burst)
		}
		r.refilled = now
	}
	if r.tokens < 1 {
		return false
	}
	r.tokens--
	return true
}

type lookupResult struct {
	pid  int
	name string
}

func (r *Recorder) lookup(c Call, at time.Time) {
	defer r.wg.Done()
	defer func() { <-r.inflight }()
	res := lookupResult{name: Unavailable}
	if r.peer != nil && r.procs != nil && c.ClientPort > 0 {
		done := make(chan lookupResult, 1)
		go func() {
			pid, err := r.peer.OwnerPID(c.ClientPort, c.ServerPort)
			if err != nil || pid <= 0 {
				done <- lookupResult{name: Unavailable}
				return
			}
			info, err := r.procs.Info(pid)
			if err != nil || info.Comm == "" {
				done <- lookupResult{pid: pid, name: Unavailable}
				return
			}
			done <- lookupResult{pid: pid, name: info.Comm}
		}()
		select {
		case res = <-done:
		case <-time.After(r.cfg.LookupTimeout):
		}
	}
	if r.cfg.OnLookup != nil {
		r.cfg.OnLookup(c, res.pid, res.name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	ps := r.procsBy[res.name]
	if ps == nil {
		if len(r.procsBy) >= r.cfg.MaxKeys {
			return
		}
		ps = &procStat{}
		r.procsBy[res.name] = ps
	}
	ps.calls++
	ps.last = at
	if res.pid > 0 {
		ps.lastPID = res.pid
	}
}

// Flush waits for in-flight lookups (tests and shutdown).
func (r *Recorder) Flush() { r.wg.Wait() }

// AnonymousCallsFor is how many anonymous calls claimed to come from the session.
func (r *Recorder) AnonymousCallsFor(id domain.SessionID) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := r.sessions[id]; s != nil {
		return s.calls
	}
	return 0
}

// RouteObservation is one route's anonymous-call counts.
type RouteObservation struct {
	Method, Route, Class    string
	Calls, Allowed, Refused int
	LastSeen                time.Time
}

// SessionObservation is the anonymous calls that claimed one session.
type SessionObservation struct {
	Session  domain.SessionID
	Calls    int
	LastSeen time.Time
}

// ProcessObservation is the anonymous calls attributed (best effort) to one process name.
type ProcessObservation struct {
	Name     string
	Calls    int
	LastPID  int
	LastSeen time.Time
}

// Snapshot is the top-offenders view.
type Snapshot struct {
	Since                    time.Time
	Total, Allowed, Refused  int
	Overflow, DroppedLookups int
	Routes                   []RouteObservation
	Sessions                 []SessionObservation
	Processes                []ProcessObservation
}

// Snapshot returns the totals and the top n routes, claimed sessions and processes, most called first.
func (r *Recorder) Snapshot(n int) Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := Snapshot{Since: r.since, Total: r.total, Allowed: r.allowed, Refused: r.refused, Overflow: r.overflow, DroppedLookups: r.dropped}
	for _, v := range r.routes {
		s.Routes = append(s.Routes, RouteObservation{Method: v.method, Route: v.route, Class: v.class, Calls: v.calls, Allowed: v.allowed, Refused: v.refused, LastSeen: v.last})
	}
	for id, v := range r.sessions {
		s.Sessions = append(s.Sessions, SessionObservation{Session: id, Calls: v.calls, LastSeen: v.last})
	}
	for name, v := range r.procsBy {
		s.Processes = append(s.Processes, ProcessObservation{Name: name, Calls: v.calls, LastPID: v.lastPID, LastSeen: v.last})
	}
	sort.Slice(s.Routes, func(i, j int) bool {
		if s.Routes[i].Calls != s.Routes[j].Calls {
			return s.Routes[i].Calls > s.Routes[j].Calls
		}
		return s.Routes[i].Route < s.Routes[j].Route
	})
	sort.Slice(s.Sessions, func(i, j int) bool {
		if s.Sessions[i].Calls != s.Sessions[j].Calls {
			return s.Sessions[i].Calls > s.Sessions[j].Calls
		}
		return s.Sessions[i].Session < s.Sessions[j].Session
	})
	sort.Slice(s.Processes, func(i, j int) bool {
		if s.Processes[i].Calls != s.Processes[j].Calls {
			return s.Processes[i].Calls > s.Processes[j].Calls
		}
		return s.Processes[i].Name < s.Processes[j].Name
	})
	if n > 0 {
		if len(s.Routes) > n {
			s.Routes = s.Routes[:n]
		}
		if len(s.Sessions) > n {
			s.Sessions = s.Sessions[:n]
		}
		if len(s.Processes) > n {
			s.Processes = s.Processes[:n]
		}
	}
	return s
}
