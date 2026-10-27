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
