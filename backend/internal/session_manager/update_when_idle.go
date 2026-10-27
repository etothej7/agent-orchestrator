package sessionmanager

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/persistenthost"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/procmem"
)

// ChatUpdateHibernator stops an idle Chat provider so an installer can replace
// its executable.
type ChatUpdateHibernator interface {
	HibernateChatForUpdate(ctx context.Context, id domain.SessionID) (bool, error)
}

// updateStops holds the terminal input gates of terminals stopped for an
// update, so nothing typed is lost before they resume.
type updateStops struct {
	mu     sync.Mutex
	drains map[domain.SessionID]func()
}

func (u *updateStops) hold(id domain.SessionID, release func()) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.drains == nil {
		u.drains = make(map[domain.SessionID]func())
	}
	if previous := u.drains[id]; previous != nil {
		previous()
	}
	u.drains[id] = release
}

func (u *updateStops) release(id domain.SessionID) {
	u.mu.Lock()
	release := u.drains[id]
	delete(u.drains, id)
	u.mu.Unlock()
	if release != nil {
		release()
	}
}

// liveHarnessSessions lists the sessions whose process runs harness.
func (m *Manager) liveHarnessSessions(ctx context.Context, harness domain.AgentHarness) ([]domain.SessionRecord, error) {
	recs, err := m.store.ListAllSessions(ctx)
	if err != nil {
		return nil, err
	}
	var live []domain.SessionRecord
	for _, rec := range recs {
		if rec.Harness == harness && !rec.IsTerminated && !rec.IsTaskPreparation &&
			rec.HibernatedAt == nil && rec.Activity.State != domain.ActivityExited {
			live = append(live, rec)
		}
	}
	return live, nil
}

// BusyHarnessSessions lists live sessions of harness that an update may not
// stop yet: starting, taking a turn, waiting for the user, or (for terminals)
// running a command such as a dev server.
func (m *Manager) BusyHarnessSessions(ctx context.Context, harness domain.AgentHarness) ([]domain.SessionWait, error) {
	live, err := m.liveHarnessSessions(ctx, harness)
	if err != nil {
		return nil, err
	}
	var busy []domain.SessionWait
	for _, rec := range live {
		if reason, ok := m.updateBlocker(ctx, rec); ok {
			busy = append(busy, domain.SessionWait{SessionID: rec.ID, Reason: reason})
		}
	}
	return busy, nil
}

func (m *Manager) updateBlocker(ctx context.Context, rec domain.SessionRecord) (domain.SessionWaitReason, bool) {
	switch {
	case rec.ProvisionState.IsProvisioning():
		return domain.SessionWaitStarting, true
	case rec.Activity.State == domain.ActivityActive:
		return domain.SessionWaitWorking, true
	case rec.Activity.State == domain.ActivityBlocked || rec.Activity.State == domain.ActivityWaitingInput:
		return domain.SessionWaitApproval, true
	case rec.Activity.State != domain.ActivityIdle:
		return domain.SessionWaitWorking, true
	}
	if domain.NormalizeSessionMode(rec.Mode) == domain.SessionModeTUI {
		inspector, ok := m.runtime.(ports.WorkloadCommandInspector)
		if !ok {
			return domain.SessionWaitCommand, true
		}
		commands, err := inspector.RunningWorkloadCommands(ctx, runtimeHandle(rec.Metadata), ports.SupervisedProcessRef{
			SessionID: rec.ID, LaunchID: rec.Metadata.RuntimeLaunchID,
		})
		if err != nil || commands > 0 {
			return domain.SessionWaitCommand, true
		}
		// A stopped terminal comes back only by resuming its conversation.
		if !m.terminalResumesNatively(ctx, rec) {
			return domain.SessionWaitResume, true
		}
	}
	return "", false
}

// terminalResumesNatively reports whether a terminal stopped for an update
// could start again on its own conversation, as resumeAfterUpdate requires:
// the adapter has a restore command and the conversation was persisted.
func (m *Manager) terminalResumesNatively(ctx context.Context, rec domain.SessionRecord) bool {
	agent, ok := m.agents.Agent(rec.Harness)
	if !ok {
		return false
	}
	project, err := m.loadProject(ctx, rec.ProjectID)
	if err != nil {
		return false
	}
	ref := ports.SessionRef{
		ID:            string(rec.ID),
		WorkspacePath: rec.Metadata.WorkspacePath,
		Metadata:      map[string]string{ports.MetadataKeyAgentSessionID: rec.Metadata.AgentSessionID},
	}
	agentConfig := restoredAgentConfig(rec, project.Config)
	_, ok, err = agent.GetRestoreCommand(ctx, ports.RestoreConfig{
		Session: ref, Kind: rec.Kind, DataDir: m.dataDir, Config: agentConfig, Permissions: agentConfig.Permissions,
	})
	if err != nil || !ok {
		return false
	}
	// A probe environment: nothing launches here, so it must not mint an identity proof (which
	// would replace the proof of a session whose process may still be running).
	env := m.runtimeEnv(rec.ID, rec.ProjectID, rec.IssueID, project.Config.Env)
	m.augmentAgentRuntimeEnv(agent, env)
	return !nativeConversationMissing(ctx, agent, ref, rec.Metadata.AgentSessionID, env)
}

// StopHarnessSessions stops every live session of harness without ending it:
// Chat providers are put to sleep (a message sent meanwhile queues and wakes
// the session afterwards), and terminal agents are stopped with their input
// held. It stops nothing more once a session turns out to be busy.
func (m *Manager) StopHarnessSessions(ctx context.Context, harness domain.AgentHarness) ([]domain.SessionID, error) {
	live, err := m.liveHarnessSessions(ctx, harness)
	if err != nil {
		return nil, err
	}
	var stopped []domain.SessionID
	for _, rec := range live {
		if err := m.stopForUpdate(ctx, rec); err != nil {
			return stopped, fmt.Errorf("stop %s for update: %w", rec.ID, err)
		}
		stopped = append(stopped, rec.ID)
	}
	return stopped, nil
}

func (m *Manager) stopForUpdate(ctx context.Context, rec domain.SessionRecord) error {
	if err := m.beginAgentOperation(ctx, rec.ID, agentOperationResume); err != nil {
		return err
	}
	defer m.endAgentOperation(rec.ID, agentOperationResume)
	if active, err := m.hasActiveInterfaceTransition(ctx, rec.ID); err != nil || active {
		return errors.Join(err, ErrInterfaceTransitionInProgress)
	}
	if domain.NormalizeSessionMode(rec.Mode) == domain.SessionModeChat {
		hibernator, ok := m.chat.(ChatUpdateHibernator)
		if !ok {
			return ErrInterfaceHandoffUnsupported
		}
		stopped, err := hibernator.HibernateChatForUpdate(ctx, rec.ID)
		if err != nil {
			return err
		}
		if !stopped {
			return fmt.Errorf("%w: chat has work in progress", errUpdateBusy)
		}
		return nil
	}
	lastInputAt, releaseInput := m.beginTerminalInputDrain(rec)
	current, err := m.getRecord(ctx, rec.ID)
	if err == nil && (current.Metadata.RuntimeLaunchID != rec.Metadata.RuntimeLaunchID || !m.terminalIdleForRestart(ctx, current, lastInputAt)) {
		err = fmt.Errorf("%w: terminal received input", errUpdateBusy)
	}
	if err == nil {
		if _, busy := m.updateBlocker(ctx, current); busy {
			err = fmt.Errorf("%w: terminal became busy", errUpdateBusy)
		}
	}
	if err == nil {
		err = m.stopSourceControllerConclusive(current)
	}
	if err != nil {
		if releaseInput != nil {
			releaseInput()
		}
		return err
	}
	if releaseInput != nil {
		m.updateStops.hold(rec.ID, releaseInput)
	}
	return nil
}

// ResumeHarnessSessions starts sessions stopped for an update again on the
// installed release, resuming each conversation natively.
func (m *Manager) ResumeHarnessSessions(ctx context.Context, ids []domain.SessionID) error {
	var errs []error
	for _, id := range ids {
		if err := m.resumeAfterUpdate(ctx, id); err != nil {
			// The sweep that retries failed version moves starts it later, so
			// a transient failure does not leave the session stopped.
			m.versionRetries.note(id)
			m.logger.Warn("could not resume a session after a harness update; retrying", "sessionID", id, "error", err)
			errs = append(errs, fmt.Errorf("resume %s after update: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

func (m *Manager) resumeAfterUpdate(ctx context.Context, id domain.SessionID) error {
	defer m.updateStops.release(id)
	rec, err := m.getRecord(ctx, id)
	if err != nil {
		return err
	}
	if rec.IsTerminated {
		return nil
	}
	if domain.NormalizeSessionMode(rec.Mode) == domain.SessionModeChat {
		// A wake that failed during the install cleared the hibernation mark
		// but started nothing, so only a live provider is already resumed.
		if rec.HibernatedAt == nil && m.chat != nil && m.chat.HasLiveChatController(id) {
			return nil
		}
		if err := m.WakeHibernatedChat(ctx, id); err != nil {
			return err
		}
		// Messages sent while it was stopped stayed queued; deliver them now,
		// as a wake does. A wake that already delivered them leaves none.
		return m.chat.DrainChatQueue(ctx, id)
	}
	if err := m.beginAgentOperation(ctx, id, agentOperationResume); err != nil {
		return err
	}
	defer m.endAgentOperation(id, agentOperationResume)
	releaseHarness, err := m.beginHarnessUse(rec.Harness)
	if err != nil {
		return err
	}
	defer releaseHarness()
	// The input gate stays closed until the new process is up.
	_, err = m.resumeAgentRecordWithPolicy(ctx, "resume after update", rec, true)
	result := "succeeded"
	if err != nil {
		result = "failed"
	}
	m.emitVersionSwitch(rec, "update_when_idle", result)
	return err
}

// ErrRelaunchBusy means the session is not idle (a turn is running, an approval or input is
// pending, a terminal received input), so it was not relaunched.
var ErrRelaunchBusy = errors.New("session is busy")

// ErrRelaunchWorkRunning means the session is idle but its agent has work running in the
// background (a test run, a dev server, a build, a watcher): a relaunch would kill it, so the
// session is left alone. It is not a failure; the caller waits and asks again later.
var ErrRelaunchWorkRunning = errors.New("session has background work running")

// ErrRelaunchResumeFailed means the session WAS stopped for the relaunch but could not be started
// again. It is down until something resumes it, so the caller must not treat it as a skip.
var ErrRelaunchResumeFailed = errors.New("session was stopped but could not be started again")

// chatWorkloadCommands counts the processes a chat session's agent has running below its provider
// host. An error means the count could not be proved.
func (m *Manager) chatWorkloadCommands(ctx context.Context, id domain.SessionID) (int, error) {
	if m.chatWorkloads != nil {
		return m.chatWorkloads(ctx, id)
	}
	host, ok := persistenthost.HostPID(m.dataDir, string(id))
	if !ok {
		return 0, errors.New("chat host is not running")
	}
	table, err := procmem.Snapshot(ctx, nil)
	if err != nil {
		return 0, err
	}
	return table.ShellWorkloads(host), nil
}

// RelaunchForIdentity stops one IDLE session and starts it again resuming its conversation, so the
// new process is launched with an identity proof in its environment (ED8: a session that began
// before proofs arrived has none, and only a new launch can give it one). It never touches a
// session that is not idle: the check runs first and again under the session's own lock while it
// stops (stopForUpdate), and a refusal at either point is ErrRelaunchBusy, not a failure. It also
// never kills an agent's background work: a session with commands or processes running below its
// agent is ErrRelaunchWorkRunning. A terminated or sleeping session is left alone (a sleeping one
// gets its proof when it wakes) and reports false. It uses exactly the stop and resume steps of a
// harness update. A resume that fails after the stop is ErrRelaunchResumeFailed (wrapping the
// cause): the session is down and the caller must bring it back (ResumeStoppedForIdentity).
func (m *Manager) RelaunchForIdentity(ctx context.Context, id domain.SessionID) (bool, error) {
	rec, err := m.getRecord(ctx, id)
	if err != nil {
		return false, err
	}
	if rec.IsTerminated || rec.HibernatedAt != nil {
		return false, nil
	}
	if reason, busy := m.updateBlocker(ctx, rec); busy {
		if reason == domain.SessionWaitCommand {
			return false, ErrRelaunchWorkRunning
		}
		return false, ErrRelaunchBusy
	}
	if domain.NormalizeSessionMode(rec.Mode) == domain.SessionModeChat {
		// A count that cannot be proved is treated as work running: never stop what may be busy.
		if n, err := m.chatWorkloadCommands(ctx, id); err != nil || n > 0 {
			return false, ErrRelaunchWorkRunning
		}
	}
	if err := m.stopForUpdate(ctx, rec); err != nil {
		if errors.Is(err, errUpdateBusy) || errors.Is(err, ErrInterfaceTransitionInProgress) {
			return false, ErrRelaunchBusy
		}
		return false, err
	}
	if err := m.resumeAfterUpdate(ctx, id); err != nil {
		m.logger.Warn("could not start a session again after relaunching it for an identity proof", "sessionID", id, "error", err)
		return false, fmt.Errorf("%w: %w", ErrRelaunchResumeFailed, err)
	}
	return true, nil
}

// ResumeStoppedForIdentity starts a session that a relaunch stopped and could not start again. It
// reports false, nil when the session no longer needs it (it came back on its own, was resumed by
// the person, or ended).
func (m *Manager) ResumeStoppedForIdentity(ctx context.Context, id domain.SessionID) (bool, error) {
	rec, err := m.getRecord(ctx, id)
	if err != nil {
		return false, err
	}
	if rec.IsTerminated || !versionMoveStopped(rec, m.chat) {
		return false, nil
	}
	if err := m.resumeAfterUpdate(ctx, id); err != nil {
		return false, err
	}
	return true, nil
}

// errUpdateBusy marks the refusals of stopForUpdate that mean "not idle right now".
var errUpdateBusy = errors.New("session is not idle")
