package sessionmanager

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// failBootReconciliation puts the manager in the state a real boot leaves it in
// when background reconciliation returned an error: every row with no recovery
// entry reads "unavailable", exited rows included.
func failBootReconciliation(m *Manager) {
	m.statusRecoveryMu.Lock()
	m.statusRecoveryFailed = true
	m.statusRecoveryMu.Unlock()
	m.startupBackgroundReconcileOnce.Do(func() { close(m.startupBackgroundReconcileDone) })
}

func TestResumeAgentStrictResumesTheSavedChatConversationWithoutTheHandoffBarrier(t *testing.T) {
	launcher := &recordingLauncher{}
	mgr, store, _ := newChatManager(launcher)
	seedChatResumeSession(store, domain.ActivityExited)

	if _, err := mgr.ResumeAgentWithMode(context.Background(), "mer-1"); err != nil {
		t.Fatalf("ResumeAgentWithMode: %v", err)
	}
	if len(launcher.started) != 1 || launcher.started[0].HistoryMode == ports.ChatHistoryRequired {
		t.Fatalf("the ordinary resume must not require native history (it may import or start fresh): %+v", launcher.started)
	}

	launcher2 := &recordingLauncher{}
	mgr2, store2, _ := newChatManager(launcher2)
	seedChatResumeSession(store2, domain.ActivityExited)
	if _, err := mgr2.ResumeAgentStrict(context.Background(), "mer-1"); err != nil {
		t.Fatalf("ResumeAgentStrict: %v", err)
	}
	// A Chat restart is a CONTEXT resume of the saved provider conversation: typed
	// replay is imported when the driver has it and tolerated when it does not.
	// ChatHistoryRequired is the TUI->Chat handoff barrier (checkpoint gates,
	// settle budget) and is not what restarting a session that already runs in
	// Chat needs.
	if len(launcher2.started) != 1 || launcher2.started[0].HistoryMode != ports.ChatHistoryImport {
		t.Fatalf("the unattended chat resume must import history where available: %+v", launcher2.started)
	}
	if got := launcher2.started[0].ProviderConversationID; got != "thread-existing" {
		t.Fatalf("strict resume must continue the saved provider conversation, got %q", got)
	}
	if launcher2.started[0].FreshIfProviderConversationMissing {
		t.Fatal("the unattended chat resume may never start a fresh conversation in place of the saved one")
	}
}

// A saved conversation that cannot be resumed must end the attempt, never turn into a
// fresh launch that re-runs the original task.
func TestResumeAgentStrictWithMissingNativeHistoryFailsWithoutAFreshLaunch(t *testing.T) {
	launcher := &recordingLauncher{startErr: fmt.Errorf("resume: %w", ports.ErrChatResumeFailed)}
	mgr, store, runtime := newChatManager(launcher)
	seedChatResumeSession(store, domain.ActivityExited)

	_, err := mgr.ResumeAgentStrict(context.Background(), "mer-1")
	if err == nil {
		t.Fatal("a strict resume with no native history succeeded")
	}
	if ClassifyResumeFailure(err) != ResumeFailureNeedsPerson || ResumeFailureReason(err) != ResumeReasonHistory {
		t.Fatalf("a missing conversation must need a person (class=%v reason=%q): %v", ClassifyResumeFailure(err), ResumeFailureReason(err), err)
	}
	if len(launcher.started) != 1 || launcher.started[0].FreshIfProviderConversationMissing {
		t.Fatalf("exactly one start attempt expected, never fresh, no fresh retry: %+v", launcher.started)
	}
	if runtime.created != 0 {
		t.Fatalf("a terminal runtime was created for a chat resume: %d", runtime.created)
	}
	if got := store.sessions["mer-1"]; got.Activity.State != domain.ActivityExited || got.IsTerminated {
		t.Fatalf("a refused resume changed the session: %+v", got)
	}
}

// Readiness reads "unavailable" for any row with no recovery entry once boot
// reconciliation failed, exited rows included, and the ordinary resume then
// RECONCILES the live session (which relaunches non-strictly) instead of resuming
// it. The strict resume must skip that branch; the existing strict caller (the
// version refresh) keeps it.
func TestResumeAgentStrictSkipsTheReadinessReconcileBranchButTheVersionRefreshKeepsIt(t *testing.T) {
	launcher := &recordingLauncher{}
	mgr, store, _ := newChatManager(launcher)
	seedChatResumeSession(store, domain.ActivityExited)
	failBootReconciliation(mgr)
	if got := mgr.SessionStatusReadiness(store.sessions["mer-1"]); got != "unavailable" {
		t.Fatalf("setup: readiness of an exited row after a failed boot reconcile = %q, want unavailable", got)
	}

	if _, err := mgr.ResumeAgentStrict(context.Background(), "mer-1"); err != nil {
		t.Fatalf("ResumeAgentStrict: %v", err)
	}
	if len(launcher.started) != 1 || launcher.started[0].FreshIfProviderConversationMissing {
		t.Fatalf("the strict resume must have started a never-fresh controller: %+v", launcher.started)
	}
	mgr.statusRecoveryMu.RLock()
	_, reconciled := mgr.statusRecoveries["mer-1"]
	mgr.statusRecoveryMu.RUnlock()
	if reconciled {
		t.Fatal("the strict resume took the readiness-reconcile branch")
	}

	// The version refresh calls resumeAgentWithMode(ctx, id, true) directly: it still
	// takes the readiness branch.
	launcher2 := &recordingLauncher{}
	mgr2, store2, _ := newChatManager(launcher2)
	seedChatResumeSession(store2, domain.ActivityExited)
	failBootReconciliation(mgr2)
	_, _ = mgr2.resumeAgentWithMode(context.Background(), "mer-1", true)
	mgr2.statusRecoveryMu.RLock()
	_, reconciled = mgr2.statusRecoveries["mer-1"]
	mgr2.statusRecoveryMu.RUnlock()
	if !reconciled {
		t.Fatal("resumeAgentWithMode(…, true) no longer takes the readiness-reconcile branch the version refresh relies on")
	}
}

func TestResumeAgentStrictWithMissingHistoryAndUnavailableReadinessStillStartsNothingFresh(t *testing.T) {
	launcher := &recordingLauncher{startErr: ports.ErrChatResumeFailed}
	mgr, store, _ := newChatManager(launcher)
	seedChatResumeSession(store, domain.ActivityExited)
	failBootReconciliation(mgr)

	_, err := mgr.ResumeAgentStrict(context.Background(), "mer-1")
	if !errors.Is(err, ports.ErrChatResumeFailed) {
		t.Fatalf("error = %v, want the resume failure", err)
	}
	for _, start := range launcher.started {
		if start.FreshIfProviderConversationMissing {
			t.Fatalf("a fresh start was attempted with readiness unavailable and the conversation missing: %+v", launcher.started)
		}
	}
}

func TestClassifyResumeFailure(t *testing.T) {
	wrap := func(err error) error { return fmt.Errorf("resume agent mer-1: %w", err) }
	cases := []struct {
		name string
		err  error
		want ResumeFailure
	}{
		{"already running", wrap(ErrAgentNotExited), ResumeFailureAlreadyRunning},
		{"resume in progress", wrap(ErrResumeInProgress), ResumeFailureTransient},
		{"deadline", wrap(context.DeadlineExceeded), ResumeFailureTransient},
		{"driver unavailable", wrap(ports.ErrChatDriverUnavailable), ResumeFailureTransient},
		{"recovery inconclusive", wrap(ports.ErrChatRecoveryInconclusive), ResumeFailureTransient},
		{"sign-in", wrap(ports.ErrChatAuthRequired), ResumeFailureNeedsPerson},
		{"account changed", wrap(ports.ErrAgentAccountChanged), ResumeFailureNeedsPerson},
		{"binary missing", wrap(ports.ErrAgentBinaryNotFound), ResumeFailureNeedsPerson},
		{"not resumable", wrap(ErrNotResumable), ResumeFailureNeedsPerson},
		{"history unavailable", wrap(ports.ErrChatHistoryUnavailable), ResumeFailureNeedsPerson},
		{"history unsettled", wrap(ports.ErrChatHistoryUnsettled), ResumeFailureNeedsPerson},
		{"native history unavailable", wrap(ports.ErrNativeHistoryUnavailable), ResumeFailureNeedsPerson},
		{"terminated", wrap(ErrTerminated), ResumeFailurePermanent},
		{"not found", wrap(ErrNotFound), ResumeFailurePermanent},
		{"incomplete handle", wrap(ErrIncompleteHandle), ResumeFailurePermanent},
		{"something else", errors.New("boom"), ResumeFailureUnknown},
		{"a missing worktree is permanent", wrap(ErrWorkspaceMissing), ResumeFailurePermanent},
	}
	for _, c := range cases {
		if got := ClassifyResumeFailure(c.err); got != c.want {
			t.Errorf("%s: class = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestResumeFailureReasonIsAFixedPhraseNeverRawErrorText(t *testing.T) {
	raw := errors.New("sk-ant-secret-token exploded at /Users/ed/secret/path")
	reasons := map[string]ResumeReason{
		"sign-in":  ResumeFailureReason(fmt.Errorf("%w: %w", ports.ErrChatAuthRequired, raw)),
		"account":  ResumeFailureReason(fmt.Errorf("%w: %w", ports.ErrAgentAccountChanged, raw)),
		"provider": ResumeFailureReason(fmt.Errorf("%w: %w", ports.ErrAgentBinaryNotFound, raw)),
		"history":  ResumeFailureReason(fmt.Errorf("%w: %w", ErrNotResumable, raw)),
		"generic":  ResumeFailureReason(raw),
	}
	want := map[string]ResumeReason{
		"sign-in": ResumeReasonSignIn, "account": ResumeReasonAccount, "provider": ResumeReasonProvider,
		"history": ResumeReasonHistory, "generic": ResumeReasonUnresumable,
	}
	for name, got := range reasons {
		if got != want[name] {
			t.Errorf("%s: reason = %q, want %q", name, got, want[name])
		}
		if len(got) > 60 {
			t.Errorf("%s: reason %q is not a short fixed phrase", name, got)
		}
	}
}

// The unattended entry point refuses by itself, not only through the resumer's pre-check.
func TestResumeAgentStrictRefusesAProtectedOrStoppedSessionItself(t *testing.T) {
	cases := map[string]func(*fakeStore){
		"protected":         func(st *fakeStore) { st.protected = map[domain.SessionID]bool{"mer-1": true} },
		"policy unreadable": func(st *fakeStore) { st.policyUnreadable = true },
		"stopped on purpose": func(st *fakeStore) {
			st.stopReasons = map[domain.SessionID]domain.StopReason{"mer-1": domain.StopReasonUser}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			launcher := &recordingLauncher{}
			mgr, store, rt := newChatManager(launcher)
			seedChatResumeSession(store, domain.ActivityExited)
			mutate(store)
			_, err := mgr.ResumeAgentStrict(context.Background(), "mer-1")
			if ClassifyResumeFailure(err) != ResumeFailureRefused {
				t.Fatalf("error = %v (class %v), want a silent refusal", err, ClassifyResumeFailure(err))
			}
			if len(launcher.started) != 0 || rt.created != 0 {
				t.Fatalf("a refused resume launched something: %d %d", len(launcher.started), rt.created)
			}
		})
	}
}

func TestClassifyResumeFailureOfNilIsNone(t *testing.T) {
	if got := ClassifyResumeFailure(nil); got != ResumeFailureNone {
		t.Fatalf("ClassifyResumeFailure(nil) = %v, want ResumeFailureNone", got)
	}
}

// A worktree that is gone is never recreated by an unattended resume: it ends in a
// permanent, named failure instead of a launch error retried to the attempt cap.
func TestResumeAgentStrictWithAMissingWorktreeIsPermanentAndLaunchesNothing(t *testing.T) {
	launcher := &recordingLauncher{}
	mgr, store, rt := newChatManager(launcher)
	seedChatResumeSession(store, domain.ActivityExited)
	mgr.workspaceExistsFn = func(string) bool { return false }

	_, err := mgr.ResumeAgentStrict(context.Background(), "mer-1")
	if !errors.Is(err, ErrWorkspaceMissing) || ClassifyResumeFailure(err) != ResumeFailurePermanent {
		t.Fatalf("error = %v (class %v), want a permanent ErrWorkspaceMissing", err, ClassifyResumeFailure(err))
	}
	if len(launcher.started) != 0 || rt.created != 0 {
		t.Fatalf("a missing worktree launched something: %d %d", len(launcher.started), rt.created)
	}
}
