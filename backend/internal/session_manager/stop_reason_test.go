package sessionmanager

import (
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func stopReasonWorker(id string, state domain.ActivityState) domain.SessionRecord {
	return domain.SessionRecord{
		ID: domain.SessionID(id), ProjectID: "mer", Kind: domain.KindWorker, Harness: domain.HarnessCodex,
		Activity: domain.Activity{State: state},
		Metadata: domain.SessionMetadata{
			WorkspacePath: "/ws/" + id, Branch: "ao/" + id, RuntimeHandleID: "tmux-" + id,
			RuntimeLaunchID: "launch-" + id, AgentSessionID: "native-" + id,
		},
	}
}

// The stop reason must be durable BEFORE the agent is stopped: stopping provokes the
// controller's own exited write, and a restart reads the reason next to that state.
func TestExitAgentRecordsTheStopReasonBeforeTheAgentIsStopped(t *testing.T) {
	m, st, runtime, _ := newManager()
	st.sessions["mer-1"] = stopReasonWorker("mer-1", domain.ActivityIdle)
	runtime.aliveByHandle = map[string]bool{"tmux-mer-1": true}
	var reasonWhenDestroyed domain.StopReason
	runtime.onDestroy = func(int, ports.RuntimeHandle) { reasonWhenDestroyed = st.stopReasons["mer-1"] }

	if _, err := m.ExitAgent(ctx, "mer-1"); err != nil {
		t.Fatalf("ExitAgent: %v", err)
	}
	if reasonWhenDestroyed != domain.StopReasonUser {
		t.Fatalf("stop reason when the agent was stopped = %q, want %q recorded first", reasonWhenDestroyed, domain.StopReasonUser)
	}
	if got := st.stopReasons["mer-1"]; got != domain.StopReasonUser {
		t.Fatalf("stop reason after exit = %q, want %q kept so a restart does not resume it", got, domain.StopReasonUser)
	}
}

func TestExitAgentsOnAccountRecordsAnAccountStopOnlyForThoseAgents(t *testing.T) {
	m, st, runtime, _ := newManager()
	st.sessions["mer-1"] = stopReasonWorker("mer-1", domain.ActivityIdle)
	st.sessions["mer-2"] = stopReasonWorker("mer-2", domain.ActivityIdle)
	runtime.aliveByHandle = map[string]bool{"tmux-mer-1": true, "tmux-mer-2": true}
	var log []string
	accounts := newTransitionAccounts(&log)
	accounts.bound = map[domain.SessionID]string{"mer-1": "acct-b", "mer-2": "device-claude"}
	m.SetAgentAccounts(accounts)

	if err := m.ExitAgentsOnAccount(ctx, "acct-b"); err != nil {
		t.Fatal(err)
	}
	if got := st.stopReasons["mer-1"]; got != domain.StopReasonAccount {
		t.Fatalf("mer-1 stop reason = %q, want %q", got, domain.StopReasonAccount)
	}
	if got, ok := st.stopReasons["mer-2"]; ok {
		t.Fatalf("mer-2 was not stopped but has a stop reason %q", got)
	}
}

func TestAnExitThatFailsLeavesNoStopReasonBehind(t *testing.T) {
	m, st, runtime, _ := newManager()
	st.sessions["mer-1"] = stopReasonWorker("mer-1", domain.ActivityIdle)
	runtime.aliveByHandle = map[string]bool{"tmux-mer-1": true}
	runtime.destroyErr = errors.New("tmux refused")

	if _, err := m.ExitAgent(ctx, "mer-1"); err == nil {
		t.Fatal("ExitAgent succeeded although the runtime could not be stopped")
	}
	if got, ok := st.stopReasons["mer-1"]; ok {
		t.Fatalf("a failed exit left stop reason %q: a later crash would be hidden from auto-resume", got)
	}
}

func TestExitAgentRefusesWhenItCannotRecordWhyItStopped(t *testing.T) {
	m, st, runtime, _ := newManager()
	st.sessions["mer-1"] = stopReasonWorker("mer-1", domain.ActivityIdle)
	runtime.aliveByHandle = map[string]bool{"tmux-mer-1": true}
	st.stopReasonErr = errors.New("database is locked")

	_, err := m.ExitAgent(ctx, "mer-1")
	if err == nil {
		t.Fatal("ExitAgent stopped an agent without being able to record why")
	}
	if runtime.destroyed != 0 {
		t.Fatalf("the agent was stopped (%d destroys) although its stop could not be recorded: it could later be resumed against Ed's wish", runtime.destroyed)
	}
	if got := st.sessions["mer-1"]; got.Activity.State != domain.ActivityIdle {
		t.Fatalf("the session changed after a refused exit: %+v", got.Activity)
	}
}
