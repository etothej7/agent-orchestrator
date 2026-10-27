package sessionmanager

import (
	"context"
	"errors"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// ErrSessionProtected means the session's auto-resume policy is never (or could not
// be read), so AO must not replace it, tear it down or remove its worktree on its
// own. Ed lifts the protection explicitly with `ao session set-auto-resume <id>
// allowed`.
var ErrSessionProtected = errors.New("session is protected: its auto-resume policy is never")

// autoResumePolicyStore is the optional part of the store that answers the policy.
// AutoResumeAllowed fails closed: a read error is "not allowed".
type autoResumePolicyStore interface {
	AutoResumeAllowed(ctx context.Context, id domain.SessionID) bool
}

// SessionProtected reports whether the session must not be replaced, torn down or
// have its worktree removed by AO. It protects on anything but an explicit
// allowance, so an unreadable policy protects. A store that keeps no policy (the
// test fakes) protects nothing.
func (m *Manager) SessionProtected(ctx context.Context, id domain.SessionID) bool {
	store, ok := m.store.(autoResumePolicyStore)
	return ok && !store.AutoResumeAllowed(ctx, id)
}
