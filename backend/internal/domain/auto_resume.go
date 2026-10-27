package domain

import "time"

// AutoResumePolicy says whether AO may restart a session's agent on its own after
// a restart. NEVER also protects the session from replacement and teardown. A
// session with no stored policy is allowed; anything unreadable or unknown is
// treated as never (fail closed).
type AutoResumePolicy string

// The auto-resume policies.
const (
	AutoResumeAllowed AutoResumePolicy = "allowed"
	AutoResumeNever   AutoResumePolicy = "never"
)

// Valid reports whether p is a policy value that may be stored.
func (p AutoResumePolicy) Valid() bool { return p == AutoResumeAllowed || p == AutoResumeNever }

// Allows reports whether automation may resume the session. Only an explicit
// allowed qualifies, so a corrupt or unknown value protects the session.
func (p AutoResumePolicy) Allows() bool { return p == AutoResumeAllowed }

// StopReason records why an agent was stopped on purpose, written before the
// controller is stopped so it is durable ahead of the controller's own exited
// write. Empty means the agent ended without an intentional stop.
type StopReason string

// The stop reasons.
const (
	StopReasonNone    StopReason = ""
	StopReasonUser    StopReason = "user_stop"
	StopReasonAccount StopReason = "account_stop"
	StopReasonUpdate  StopReason = "update_stop"
)

// Valid reports whether r may be stored.
func (r StopReason) Valid() bool {
	switch r {
	case StopReasonNone, StopReasonUser, StopReasonAccount, StopReasonUpdate:
		return true
	default:
		return false
	}
}

// ResumeClass is how urgently a captured session needs its agent back.
type ResumeClass string

const (
	// ResumeWorking sessions were mid-work (or are the main coordinator) and are
	// resumed actively.
	ResumeWorking ResumeClass = "working"
	// ResumeIdle sessions were idle and are parked: the next message wakes them.
	ResumeIdle ResumeClass = "idle"
)

// ResumeState is one captured intent's progress.
type ResumeState string

// The resume intent states.
const (
	ResumePending    ResumeState = "pending"
	ResumeResuming   ResumeState = "resuming"
	ResumeResumed    ResumeState = "resumed"
	ResumeHibernated ResumeState = "hibernated"
	ResumeFailed     ResumeState = "failed"
	ResumeAbandoned  ResumeState = "abandoned"
)

// Live reports whether the intent still has work to do.
func (s ResumeState) Live() bool { return s == ResumePending || s == ResumeResuming }

// InterruptedTurn is a human prompt a restart cut off. Delivered is whether the
// provider ever confirmed it (a provider turn id exists); an undelivered turn is
// never re-sent.
type InterruptedTurn struct {
	ID        string `json:"id"`
	Delivered bool   `json:"delivered"`
}

// ResumeIntent is the durable record that a session lost its agent in a restart
// and should get it back. One per session; Revision is its compare-and-swap token.
type ResumeIntent struct {
	SessionID        SessionID         `json:"sessionId"`
	BootID           string            `json:"bootId"`
	CapturedAt       time.Time         `json:"capturedAt"`
	Class            ResumeClass       `json:"class"`
	PriorActivity    ActivityState     `json:"priorActivity"`
	InterruptedTurns []InterruptedTurn `json:"interruptedTurns,omitempty"`
	State            ResumeState       `json:"state"`
	Attempts         int               `json:"attempts"`
	NextAttemptAt    *time.Time        `json:"nextAttemptAt,omitempty"`
	LastError        string            `json:"lastError,omitempty"`
	ContinueSentAt   *time.Time        `json:"continueSentAt,omitempty"`
	// ResumedAt is when the agent came back. The continue turn's expiry runs from here,
	// not from capture: a session that resumed on a late backoff attempt still needs it.
	ResumedAt *time.Time `json:"resumedAt,omitempty"`
	// RecentResumes survives a new intent replacing an old one, so the flap guard
	// can count successful resumes across boots.
	RecentResumes []time.Time `json:"recentResumes,omitempty"`
	UpdatedAt     time.Time   `json:"updatedAt"`
	Revision      int64       `json:"-"`
}

// DaemonRun is the liveness stamp of one daemon process.
type DaemonRun struct {
	BootID            string
	StartedAt         time.Time
	HeartbeatAt       time.Time
	ShutdownStartedAt *time.Time
	EndedCleanAt      *time.Time
}

// End is the last moment the run is known to have been alive: the latest of its
// heartbeat, the start of its graceful shutdown and its clean end.
func (r DaemonRun) End() time.Time {
	end := r.HeartbeatAt
	if end.Before(r.StartedAt) {
		end = r.StartedAt
	}
	if r.ShutdownStartedAt != nil && r.ShutdownStartedAt.After(end) {
		end = *r.ShutdownStartedAt
	}
	if r.EndedCleanAt != nil && r.EndedCleanAt.After(end) {
		end = *r.EndedCleanAt
	}
	return end
}

// ShutdownWindowLookback is how far before the previous daemon's last sign of life
// an exit is still attributed to that daemon's shutdown. It is generous against a
// 10 s heartbeat plus a 10 s shutdown timeout.
const ShutdownWindowLookback = 60 * time.Second

// FirstBootFallbackMaxAge bounds how stale the first-boot fallback may be before
// the shutdown-window rule is switched off for that boot.
const FirstBootFallbackMaxAge = 10 * time.Minute

// BootWindow is what the previous daemon run left behind. It is computed ONCE, at
// the very start of boot and before any health pass, because the pass itself writes
// rows (and bumps their update times) that would otherwise drift the window to "now".
type BootWindow struct {
	BootID    string
	BootStart time.Time
	// PrevStart and PrevEnd bound the previous run. PrevEnd is clamped to BootStart
	// so a clock that jumped backwards cannot place the window in the future.
	PrevStart time.Time
	PrevEnd   time.Time
	// HasPrevious is true when a previous daemon_runs row existed.
	HasPrevious bool
	// Fallback is true when there was no previous row (the first boot after upgrading
	// from a version that wrote none) and PrevEnd is the newest session update time.
	Fallback bool
}

// NewBootWindow builds the window. prev is the previous run (nil if none) and
// newestSessionUpdate the newest session update time read before the pass (zero if
// there are no sessions).
func NewBootWindow(bootID string, bootStart time.Time, prev *DaemonRun, newestSessionUpdate time.Time) BootWindow {
	w := BootWindow{BootID: bootID, BootStart: bootStart}
	clamp := func(t time.Time) time.Time {
		if t.After(bootStart) {
			return bootStart
		}
		return t
	}
	switch {
	case prev != nil:
		w.HasPrevious = true
		w.PrevStart = clamp(prev.StartedAt)
		w.PrevEnd = clamp(prev.End())
	case !newestSessionUpdate.IsZero():
		w.Fallback = true
		w.PrevEnd = clamp(newestSessionUpdate)
	}
	return w
}

// RuleBEnabled reports whether exits recorded during the previous daemon's
// shutdown may be attributed to a restart. With no previous run and no sessions
// there is nothing to anchor on; a fallback anchor that is more than
// FirstBootFallbackMaxAge old no longer says anything about a restart.
func (w BootWindow) RuleBEnabled() bool {
	switch {
	case w.HasPrevious:
		return true
	case w.Fallback:
		return w.BootStart.Sub(w.PrevEnd) <= FirstBootFallbackMaxAge
	default:
		return false
	}
}

// ShutdownWindowStart is the earliest exit time still attributed to the previous
// daemon's shutdown: a minute before its last sign of life, but never before that
// run started (a boot that lived under a minute must not re-capture the exits its
// own health pass wrote).
func (w BootWindow) ShutdownWindowStart() time.Time {
	start := w.PrevEnd.Add(-ShutdownWindowLookback)
	if w.HasPrevious && w.PrevStart.After(start) {
		start = w.PrevStart
	}
	return start
}

// InShutdownWindow reports whether an exit at t belongs to the previous daemon's
// shutdown. The upper bound is the start of THIS boot, not the previous run's last
// stamp: the dying daemon records exits after its last heartbeat (and after its
// shutdown stamp when it is killed before its clean end), and no daemon runs
// between the two boots, so nothing else can write an exit in that gap.
func (w BootWindow) InShutdownWindow(t time.Time) bool {
	return w.RuleBEnabled() && !t.Before(w.ShutdownWindowStart()) && !t.After(w.BootStart)
}
