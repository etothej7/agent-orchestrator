package sessionmanager

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// NativeHistoryProof is what the adapter's history probe says about a session's
// saved provider conversation.
type NativeHistoryProof int

// The probe outcomes. Unproven is deliberately the zero value: no probe, a probe
// that errored and a missing session record all read as "not proven present".
const (
	NativeHistoryUnproven NativeHistoryProof = iota
	NativeHistoryPresent
	NativeHistoryMissing
)

// ProbeNativeHistory asks the harness adapter whether the session's saved provider
// conversation exists. Only an explicit "exists" is Present: the helper that boot
// otherwise uses (nativeConversationMissing) treats a missing probe or a failed one
// as present, which is the wrong default for deciding to park a session on a
// non-strict wake path.
func (m *Manager) ProbeNativeHistory(ctx context.Context, id domain.SessionID) NativeHistoryProof {
	rec, found, err := m.store.GetSession(ctx, id)
	if err != nil || !found || strings.TrimSpace(rec.Metadata.ProviderConversationID) == "" {
		return NativeHistoryUnproven
	}
	agent, ok := m.agents.Agent(rec.Harness)
	if !ok {
		return NativeHistoryUnproven
	}
	handoff, ok := agent.(ports.AgentInterfaceHandoff)
	if !ok {
		return NativeHistoryUnproven
	}
	probe, ok := handoff.(ports.AgentInterfaceHandoffHistoryProbe)
	if !ok {
		return NativeHistoryUnproven
	}
	project, err := m.loadProject(ctx, rec.ProjectID)
	if err != nil {
		return NativeHistoryUnproven
	}
	env := m.runtimeEnv(rec.ID, rec.ProjectID, rec.IssueID, project.Config.Env)
	exists, err := probe.NativeConversationExists(ctx, ports.SessionRef{
		ID:            string(rec.ID),
		WorkspacePath: rec.Metadata.WorkspacePath,
		Metadata: map[string]string{
			ports.MetadataKeyAgentSessionID: rec.Metadata.AgentSessionID,
			ports.MetadataKeySessionMode:    string(rec.Mode),
		},
	}, rec.Metadata.ProviderConversationID, env)
	switch {
	case err != nil:
		return NativeHistoryUnproven
	case exists:
		return NativeHistoryPresent
	default:
		return NativeHistoryMissing
	}
}

// sessionParker is the optional part of the store that parks a session.
type sessionParker interface {
	ParkSessionHibernated(ctx context.Context, id domain.SessionID, expectedRevision int64, at time.Time) (bool, error)
}

// ParkResumeSession puts an exited idle Chat session to sleep without launching a
// provider: one revision-checked write sets the sleep marker and the idle state, so
// the next message wakes it through the ordinary worktree-preserving path and
// every reader that treats an exited session as dead sees a live-enough one. It
// re-checks everything an unattended action must (policy fails closed, no stop
// reason, not terminated or already asleep, no live controller) and reports false
// when the row is no longer in the state the caller captured; contention is a
// retry, never "parked".
func (m *Manager) ParkResumeSession(ctx context.Context, id domain.SessionID) (bool, error) {
	parker, ok := m.store.(sessionParker)
	if !ok {
		return false, fmt.Errorf("park %s: %w", id, ports.ErrChatUnsupported)
	}
	if err := m.beginAgentResume(ctx, id); err != nil {
		return false, fmt.Errorf("park %s: %w", id, err)
	}
	defer m.endAgentResume(id)
	if m.SessionProtected(ctx, id) {
		return false, nil
	}
	if reader, ok := m.store.(resumeCaptureStore); ok {
		if reason, err := reader.StopReason(ctx, id); err != nil || reason != domain.StopReasonNone {
			return false, err
		}
	}
	rec, found, err := m.store.GetSession(ctx, id)
	if err != nil {
		return false, fmt.Errorf("park %s: %w", id, err)
	}
	if !found || rec.IsTerminated || rec.HibernatedAt != nil || domain.NormalizeSessionMode(rec.Mode) != domain.SessionModeChat ||
		rec.Activity.State != domain.ActivityExited || (m.chat != nil && m.chat.HasLiveChatController(id)) {
		return false, nil
	}
	parked, err := parker.ParkSessionHibernated(ctx, id, rec.Revision, m.clock())
	if err != nil {
		return false, fmt.Errorf("park %s: %w", id, err)
	}
	return parked, nil
}
