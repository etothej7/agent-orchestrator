package sessionmanager

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// ErrSessionProtected means the session's auto-resume policy is never (or could not
// be read), so AO must not replace it, tear it down or remove its worktree on its
// own. Ed lifts the protection explicitly, in the AO app (the session's ... menu).
var ErrSessionProtected = errors.New("session is protected: its auto-resume policy is never")

// ErrStoppedOnPurpose means a person stopped the agent (or an account or update move
// did), so an unattended resume must leave it stopped.
var ErrStoppedOnPurpose = errors.New("agent was stopped on purpose")

// ErrWorkspaceMissing means the session's worktree is gone from disk. An unattended
// resume never recreates it (that is a restore, and a person's call).
var ErrWorkspaceMissing = errors.New("session workspace is missing")

// ErrAutoResumeAuthorityUnavailable means who may change the policy could not be
// determined (the coordination config could not be read). The change is refused.
var ErrAutoResumeAuthorityUnavailable = errors.New("cannot determine who may change the auto-resume policy")

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
	if reader, ok := m.store.(autoResumePolicyReader); ok {
		policy, err := reader.AutoResumePolicy(ctx, id)
		if err != nil {
			// Fail closed, and say why once per session so an unreadable policy table is
			// visible instead of silently protecting everything.
			if _, logged := m.policyErrLogged.LoadOrStore(id, struct{}{}); !logged {
				m.logger.Warn("auto-resume policy unreadable; treating the session as protected", "sessionID", id, "error", err)
			}
			return true
		}
		return !policy.Allows()
	}
	store, ok := m.store.(autoResumePolicyStore)
	return ok && !store.AutoResumeAllowed(ctx, id)
}

// autoResumePolicyReader is the store's policy read that reports its error.
type autoResumePolicyReader interface {
	AutoResumePolicy(ctx context.Context, id domain.SessionID) (domain.AutoResumePolicy, error)
}

// ErrAutoResumeNotPermitted means the caller may not set that policy. The human
// (empty sender) may set either value; the main coordinator may set never but not
// lift it; every other session is refused, so lifting a protection is human-only.
// Cooperative, like the story done guard: the sender is attribution, not
// authentication.
var ErrAutoResumeNotPermitted = errors.New("this session may not change that auto-resume policy")

// autoResumePolicyWriter is the store's policy writer.
type autoResumePolicyWriter interface {
	SetAutoResumePolicy(ctx context.Context, id domain.SessionID, policy domain.AutoResumePolicy, now time.Time) error
}

// SetAutoResumePolicy sets a session's auto-resume policy on behalf of sender.
func (m *Manager) SetAutoResumePolicy(ctx context.Context, id domain.SessionID, policy domain.AutoResumePolicy, sender domain.SessionID) error {
	if !policy.Valid() {
		return fmt.Errorf("set auto-resume %s: invalid policy %q", id, policy)
	}
	writer, ok := m.store.(autoResumePolicyWriter)
	if !ok {
		return fmt.Errorf("set auto-resume %s: %w", id, ports.ErrChatUnsupported)
	}
	if _, found, err := m.store.GetSession(ctx, id); err != nil {
		return fmt.Errorf("set auto-resume %s: %w", id, err)
	} else if !found {
		return fmt.Errorf("set auto-resume %s: %w", id, ErrNotFound)
	}
	if sender != "" {
		if policy.Allows() {
			return fmt.Errorf("set auto-resume %s: %w", id, ErrAutoResumeNotPermitted)
		}
		isCoordinator, err := m.isMainCoordinator(ctx, sender)
		if err != nil {
			return fmt.Errorf("set auto-resume %s: %w: %w", id, ErrAutoResumeAuthorityUnavailable, err)
		}
		if !isCoordinator {
			return fmt.Errorf("set auto-resume %s: %w", id, ErrAutoResumeNotPermitted)
		}
	}
	previous := domain.AutoResumePolicy("unknown")
	if reader, ok := m.store.(autoResumePolicyReader); ok {
		if p, err := reader.AutoResumePolicy(ctx, id); err == nil {
			previous = p
		}
	}
	if err := writer.SetAutoResumePolicy(ctx, id, policy, m.clock()); err != nil {
		return fmt.Errorf("set auto-resume %s: %w", id, err)
	}
	// Every change is audited (lifting a protection most of all): who asked, and from what to what.
	byWhom := string(sender)
	if byWhom == "" {
		byWhom = "human"
	}
	m.logger.Info("auto-resume policy changed", "sessionID", id, "from", previous, "to", policy, "by", byWhom)
	if !policy.Allows() {
		// A policy change takes effect at once: a pending resume intent for a session that
		// just became protected is dropped (the resumer re-checks the policy anyway).
		m.clearAutoResumeIntent(ctx, id)
	}
	return nil
}

// isMainCoordinator reports whether the session is a live orchestrator of the
// coordinator project.
func (m *Manager) isMainCoordinator(ctx context.Context, id domain.SessionID) (bool, error) {
	cfg, err := m.coordinationConfig(ctx)
	if err != nil {
		return false, err
	}
	rec, found, err := m.store.GetSession(ctx, id)
	if err != nil || !found {
		return false, err
	}
	return cfg.CoordinatorProjectID != "" && rec.ProjectID == cfg.CoordinatorProjectID &&
		rec.Kind == domain.KindOrchestrator && !rec.IsTerminated, nil
}
