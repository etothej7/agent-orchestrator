package domain

import (
	"encoding/json"
	"time"
)

// HumanRequestKind names a human-only action that someone else asked for and the person
// confirms in the app. The daemon, not the requester, renders what the request says.
type HumanRequestKind string

const (
	// HumanRequestAutoResumeLift asks to lift a session's never protection.
	HumanRequestAutoResumeLift HumanRequestKind = "auto_resume_lift"
	// HumanRequestConversationSettings asks to change a conversation's model, reasoning
	// effort or other turn settings (never its permission mode: that is a different kind).
	HumanRequestConversationSettings HumanRequestKind = "conversation_settings"
	// HumanRequestProviderSwitch asks to move a conversation to another agent harness.
	HumanRequestProviderSwitch HumanRequestKind = "provider_switch"
	// HumanRequestPRMerge asks to merge a pull request on a project that has not let agents merge.
	HumanRequestPRMerge HumanRequestKind = "pr_merge"
	// HumanRequestProjectConfig asks to change protected settings of a project's configuration (commands that
	// run, environment, rules, permissions): fields only the person may change.
	HumanRequestProjectConfig HumanRequestKind = "project_config"
)

// HumanRequestTargetKind says what a request is about.
type HumanRequestTargetKind string

const (
	// HumanTargetSession is a request about one session.
	HumanTargetSession HumanRequestTargetKind = "session"
	// HumanTargetProject is a request about one project.
	HumanTargetProject HumanRequestTargetKind = "project"
)

// HumanRequestState is where a request is in its life.
type HumanRequestState string

const (
	// HumanRequestPending waits for the person.
	HumanRequestPending HumanRequestState = "pending"
	// HumanRequestApproved is claimed by an approval; the action is being applied.
	HumanRequestApproved HumanRequestState = "approved"
	// HumanRequestDenied was refused by the person.
	HumanRequestDenied HumanRequestState = "denied"
	// HumanRequestExpired passed its deadline before anyone decided.
	HumanRequestExpired HumanRequestState = "expired"
	// HumanRequestApplied was approved and the action done.
	HumanRequestApplied HumanRequestState = "applied"
	// HumanRequestFailed was approved but the action could not be done.
	HumanRequestFailed HumanRequestState = "failed"
)

// Valid reports whether s is a known state.
func (s HumanRequestState) Valid() bool {
	switch s {
	case HumanRequestPending, HumanRequestApproved, HumanRequestDenied, HumanRequestExpired, HumanRequestApplied, HumanRequestFailed:
		return true
	default:
		return false
	}
}

// HumanRequestRequesterKind says who filed a request.
type HumanRequestRequesterKind string

const (
	// HumanRequesterSession is an agent session. Until per-session proofs exist the id is
	// cooperative attribution: the card says who it claims to be, never more.
	HumanRequesterSession HumanRequestRequesterKind = "session"
	// HumanRequesterTerminal is a caller that named no session: "terminal (unauthenticated)".
	HumanRequesterTerminal HumanRequestRequesterKind = "terminal"
)

// HumanRequestRequester identifies who asked.
type HumanRequestRequester struct {
	Kind      HumanRequestRequesterKind `json:"kind"`
	SessionID SessionID                 `json:"sessionId,omitempty"`
}

// HumanRequestChange is one line of the diff the daemon rendered from the payload: a stable
// field key and the value before and after, as text.
type HumanRequestChange struct {
	Field string `json:"field"`
	From  string `json:"from"`
	To    string `json:"to"`
}

// HumanRequest is one pending human-only action. Summary and Changes are rendered by the
// daemon from Payload when the request is filed and never edited; PayloadHash covers the
// kind, the target and the payload, and an approval must name it.
type HumanRequest struct {
	ID   string
	Kind HumanRequestKind
	// SessionID is the session a session-targeted request is about; ProjectID is the project a
	// project-targeted one is about. Exactly one is set.
	SessionID   SessionID
	ProjectID   string
	Requester   HumanRequestRequester
	Payload     json.RawMessage
	PayloadHash string
	Summary     string
	Changes     []HumanRequestChange
	State       HumanRequestState
	Result      string
	CreatedAt   time.Time
	ExpiresAt   time.Time
	// DecidedAt is zero until someone approved, denied or the request expired.
	DecidedAt time.Time
}

// TargetKind says whether the request is about a session or a project.
func (r HumanRequest) TargetKind() HumanRequestTargetKind {
	if r.ProjectID != "" {
		return HumanTargetProject
	}
	return HumanTargetSession
}

// TargetKey names what the request is about in one string: the session id, or "project:" and the project id.
// A session id never contains a colon, so the two cannot collide; the open-request uniqueness and the payload
// hash both use it.
func (r HumanRequest) TargetKey() string {
	return HumanRequestTargetKey(r.SessionID, r.ProjectID)
}

// HumanRequestTargetKey is TargetKey for a target not yet in a request.
func HumanRequestTargetKey(session SessionID, project string) string {
	if project != "" {
		return "project:" + project
	}
	return string(session)
}
