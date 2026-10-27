package identity

import "github.com/aoagents/agent-orchestrator/backend/internal/domain"

// Verdict is the outcome of an authority decision for one request.
type Verdict int

const (
	// Refuse means the caller may not do this, and nothing should be filed on its behalf.
	Refuse Verdict = iota
	// NeedsHuman means only the person may do this. The route files a pending request for the person
	// to approve in the app instead of applying anything.
	NeedsHuman
	// Allow means the caller may do this now.
	Allow
)

func (v Verdict) String() string {
	switch v {
	case Allow:
		return "allow"
	case NeedsHuman:
		return "needs_human"
	default:
		return "refuse"
	}
}

// IsSession reports whether the caller is the verified session id: an agent whose proof was
// issued to exactly that session. The person, a reviewer and an anonymous caller never are.
func (c Caller) IsSession(id domain.SessionID) bool {
	return c.IsAgent() && id != "" && c.Session == id
}

// IsManagerOf reports whether the caller is a verified orchestrator session of the project.
func (c Caller) IsManagerOf(project domain.ProjectID) bool {
	return c.IsAgent() && c.SessionKind == domain.KindOrchestrator && project != "" && c.Project == project
}

// Target names the session an action is about: the worker whose pull request, config or
// lifecycle is touched, and its project (both from the daemon's own records).
type Target struct {
	Session domain.SessionID
	Project domain.ProjectID
}

// DecideMerge says whether the caller may merge the pull request of target.
//
// A merge acts under the person's GitHub identity, so it is the person's by default. The person
// (a verified capability) is never refused. A verified agent may merge only when the project
// turned agent merging on (agentMayMerge, off until the person enables it) AND it is the pull
// request's own worker or the project's manager; with the setting off the same agents get
// NeedsHuman, so the route files a request rather than failing a documented workflow. A reviewer
// never merges. A caller who proved nothing (a terminal, a session started before upgrade) has
// no authority: NeedsHuman, so it too files a request attributed to an unauthenticated terminal.
//
// The route must still check the preconditions the design requires even when this allows: a
// passing native review on exactly the expected head SHA and green required checks.
func DecideMerge(c Caller, agentMayMerge bool, target Target) Verdict {
	switch {
	case c.IsHuman():
		return Allow
	case c.IsReviewer():
		return Refuse
	case c.IsAgent():
		if !c.IsSession(target.Session) && !c.IsManagerOf(target.Project) {
			return Refuse
		}
		if agentMayMerge {
			return Allow
		}
		return NeedsHuman
	default:
		return NeedsHuman
	}
}

// ReviewRunRef names a review run: the review row it belongs to and the worker it reviews.
type ReviewRunRef struct {
	Review string
	Worker domain.SessionID
}

// MaySubmitReview says whether the caller may submit the review of run: only the native reviewer
// of exactly that run's review, or the person. The worker being reviewed and the project's
// manager may not (a review submitted by its own subject is not independent), and a reviewer of
// another run may not.
func MaySubmitReview(c Caller, run ReviewRunRef) bool {
	switch {
	case c.IsHuman():
		return true
	case c.IsReviewer():
		return run.Review != "" && c.Review == run.Review && (run.Worker == "" || c.Worker == run.Worker)
	default:
		return false
	}
}
