package sessionmanager

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

const preRemoveMarker = "pre-remove-ran"

func protectedKillFixture(t *testing.T, protected bool) (*Manager, *fakeStore, *fakeWorkspace, domain.SessionRecord) {
	t.Helper()
	m, st, ws, rec := newCleanupFixture(t, "echo ran > "+preRemoveMarker)
	if protected {
		st.protected = map[domain.SessionID]bool{rec.ID: true}
	}
	return m, st, ws, rec
}

func markerRan(rec domain.SessionRecord) bool {
	_, err := os.Stat(filepath.Join(rec.Metadata.WorkspacePath, preRemoveMarker))
	return err == nil
}

func TestRetireForReplacementRefusesAProtectedSession(t *testing.T) {
	m, st, rt, ws := newManager()
	st.sessions["mer-1"] = mkLive("mer-1")
	st.protected = map[domain.SessionID]bool{"mer-1": true}

	err := m.RetireForReplacement(ctx, "mer-1")
	if !errors.Is(err, ErrSessionProtected) {
		t.Fatalf("RetireForReplacement = %v, want ErrSessionProtected", err)
	}
	if ws.destroyed != 0 || rt.destroyed != 0 {
		t.Fatalf("a protected session was torn down: workspace=%d runtime=%d", ws.destroyed, rt.destroyed)
	}
	if got := st.sessions["mer-1"]; got.IsTerminated {
		t.Fatalf("a protected session was terminated by a replacement: %+v", got)
	}
}

// The protected decision must come before EVERY destroyer in kill(): the
// pre-remove user script (which may delete files before the worktree goes) and
// workspace.Destroy. The session is still terminated; only the worktree stays.
func TestKillOfAProtectedSessionTerminatesButNeverRemovesTheWorktree(t *testing.T) {
	m, st, ws, rec := protectedKillFixture(t, true)

	freed, err := m.Kill(ctx, rec.ID)
	if err != nil || freed {
		t.Fatalf("Kill = freed %v err %v, want the session terminated with its workspace preserved", freed, err)
	}
	if !st.sessions[rec.ID].IsTerminated {
		t.Fatal("Ed's explicit kill must still terminate a protected session")
	}
	if ws.destroyed != 0 {
		t.Fatalf("workspace.Destroy ran %d times for a protected session", ws.destroyed)
	}
	if markerRan(rec) {
		t.Fatal("the pre-remove script ran for a protected session: it may delete files before the worktree goes")
	}
	if _, err := os.Stat(rec.Metadata.WorkspacePath); err != nil {
		t.Fatalf("the protected worktree is gone: %v", err)
	}
}

func TestTheSameKillOfAnUnprotectedSessionStillRemovesItsWorktree(t *testing.T) {
	m, st, ws, rec := protectedKillFixture(t, false)

	freed, err := m.Kill(ctx, rec.ID)
	if err != nil || !freed || ws.destroyed != 1 || !markerRan(rec) || !st.sessions[rec.ID].IsTerminated {
		t.Fatalf("control: freed=%v err=%v destroyed=%d preRemove=%v", freed, err, ws.destroyed, markerRan(rec))
	}
}

// RequestKill is the daemon's path: it hands cleanup to a background worker, which
// a protected session must never reach.
func TestRequestKillOfAProtectedSessionNeverStartsBackgroundCleanup(t *testing.T) {
	m, st, ws, rec := protectedKillFixture(t, true)

	result, err := m.RequestKill(ctx, rec.ID)
	if err != nil || result.Freed || result.CleanupPending {
		t.Fatalf("RequestKill = %+v err %v, want terminated with nothing pending", result, err)
	}
	if !st.sessions[rec.ID].IsTerminated {
		t.Fatal("the protected session was not terminated")
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.WaitBackgroundWorkers(waitCtx); err != nil {
		t.Fatal(err)
	}
	if ws.destroyed != 0 || markerRan(rec) {
		t.Fatalf("a background cleanup touched a protected worktree: destroyed=%d preRemove=%v", ws.destroyed, markerRan(rec))
	}
}

func TestKillOfAProtectedWorkspaceProjectKeepsEveryRepo(t *testing.T) {
	m, st, _, ws := newManager()
	st.projects["mer"] = domain.ProjectRecord{ID: "mer", Path: "/repo/mer", Kind: domain.ProjectKindWorkspace, Config: testRoleAgents()}
	st.workspaceRepo["mer"] = []domain.WorkspaceRepoRecord{{Name: "api", RelativePath: "api"}}
	st.sessions["mer-1"] = domain.SessionRecord{
		ID: "mer-1", ProjectID: "mer",
		Metadata: domain.SessionMetadata{WorkspacePath: "/ws/mer-1", Branch: "ao/mer-1", RuntimeHandleID: "h1"},
		Activity: domain.Activity{State: domain.ActivityActive},
	}
	st.worktrees["mer-1"] = []domain.SessionWorktreeRecord{
		{SessionID: "mer-1", RepoName: domain.RootWorkspaceRepoName, Branch: "ao/mer-1", WorktreePath: "/ws/mer-1"},
		{SessionID: "mer-1", RepoName: "api", Branch: "ao/mer-1", WorktreePath: "/ws/mer-1/api"},
	}
	st.protected = map[domain.SessionID]bool{"mer-1": true}
	closer := &fakeShellTerminalCloser{}
	m.SetShellTerminalCloser(closer)

	freed, err := m.Kill(ctx, "mer-1")
	if err != nil || freed {
		t.Fatalf("freed=%v err=%v", freed, err)
	}
	if len(ws.calls) != 0 {
		t.Fatalf("a protected workspace project's repos were touched: %v", ws.calls)
	}
	if !st.sessions["mer-1"].IsTerminated {
		t.Fatal("the protected session was not terminated")
	}
	// The per-repo rows stay as non-restorable inventory, so the unlock can reclaim them.
	if got := len(st.worktrees["mer-1"]); got != 2 {
		t.Fatalf("the protected workspace project's inventory rows were deleted: %d left, want 2", got)
	}
	if closer.ended == nil || len(closer.began) != 1 {
		t.Fatalf("scoped shell terminals were not shut with the protected session: began=%v ended=%v", closer.began, closer.ended)
	}

	// While protected, cleanup skips it; after the unlock it reclaims every repo.
	if res, err := m.Cleanup(ctx, "mer"); err != nil || len(res.Cleaned) != 0 || len(ws.calls) != 0 {
		t.Fatalf("cleanup touched a protected session: %+v err=%v calls=%v", res, err, ws.calls)
	}
	delete(st.protected, "mer-1")
	res, err := m.Cleanup(ctx, "mer")
	if err != nil || len(res.Cleaned) != 1 {
		t.Fatalf("cleanup after the unlock = %+v err=%v, want the session reclaimed", res, err)
	}
	if want := "Destroy:api,Destroy:__root__"; strings.Join(ws.calls, ",") != want {
		t.Fatalf("destroy order after the unlock = %v, want %s", ws.calls, want)
	}
}

func TestCleanupSkipsAProtectedTerminatedSessionUntilItIsUnlocked(t *testing.T) {
	m, st, ws, rec := protectedKillFixture(t, true)
	rec.IsTerminated = true
	st.sessions[rec.ID] = rec

	result, err := m.Cleanup(ctx, rec.ProjectID)
	if err != nil || len(result.Cleaned) != 0 || len(result.Skipped) != 1 {
		t.Fatalf("Cleanup = %+v err %v, want the protected session skipped", result, err)
	}
	if reason := result.Skipped[0].Reason; !strings.Contains(reason, "protected") || !strings.Contains(reason, "AO app") {
		t.Fatalf("skip reason %q must say it is protected and how to unlock it", reason)
	}
	if ws.destroyed != 0 {
		t.Fatalf("cleanup removed a protected worktree: %d", ws.destroyed)
	}

	delete(st.protected, rec.ID) // Ed unlocks it
	result, err = m.Cleanup(ctx, rec.ProjectID)
	if err != nil || len(result.Cleaned) != 1 || ws.destroyed != 1 {
		t.Fatalf("after the unlock cleanup must reclaim it: %+v err %v destroyed=%d", result, err, ws.destroyed)
	}
}

// An unreadable policy must protect, never allow.
func TestAnUnreadablePolicyProtectsTheSession(t *testing.T) {
	m, st, ws, rec := protectedKillFixture(t, false)
	st.policyUnreadable = true

	if !m.SessionProtected(ctx, rec.ID) {
		t.Fatal("an unreadable policy was treated as an allowance")
	}
	if err := m.RetireForReplacement(ctx, rec.ID); !errors.Is(err, ErrSessionProtected) {
		t.Fatalf("RetireForReplacement = %v, want ErrSessionProtected", err)
	}
	if freed, err := m.Kill(ctx, rec.ID); err != nil || freed || ws.destroyed != 0 || markerRan(rec) {
		t.Fatalf("an unreadable policy let a kill remove the worktree: freed=%v err=%v destroyed=%d", freed, err, ws.destroyed)
	}
}

// An unreadable policy fails closed and is logged once per session.
type failingPolicyStore struct {
	*fakeStore
	reads int
}

func (s *failingPolicyStore) AutoResumePolicy(context.Context, domain.SessionID) (domain.AutoResumePolicy, error) {
	s.reads++
	return domain.AutoResumeNever, errors.New("policy table unreadable")
}

func TestSessionProtectedLogsAnUnreadablePolicyOncePerSession(t *testing.T) {
	var logs strings.Builder
	m, st, _, _ := newManager()
	m.logger = slog.New(slog.NewTextHandler(&logs, nil))
	m.store = &failingPolicyStore{fakeStore: st}
	for range 3 {
		if !m.SessionProtected(ctx, "mer-1") {
			t.Fatal("an unreadable policy must protect")
		}
	}
	if got := strings.Count(logs.String(), "policy unreadable"); got != 1 {
		t.Fatalf("logged %d times, want once:\n%s", got, logs.String())
	}
	m.SessionProtected(ctx, "mer-2")
	if got := strings.Count(logs.String(), "policy unreadable"); got != 2 {
		t.Fatalf("a second session was not logged: %d", got)
	}
}

// The shutdown save path force-removes worktrees, so it must skip a protected session.
func TestSaveAndTeardownAllSkipsAProtectedSession(t *testing.T) {
	m, st, _, ws := newManager()
	st.sessions["mer-1"] = domain.SessionRecord{
		ID: "mer-1", ProjectID: "mer",
		Metadata: domain.SessionMetadata{WorkspacePath: "/ws/mer-1", Branch: "ao/mer-1", RuntimeHandleID: "h1"},
		Activity: domain.Activity{State: domain.ActivityActive},
	}
	st.protected = map[domain.SessionID]bool{"mer-1": true}
	if err := m.SaveAndTeardownAll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(ws.calls) != 0 || ws.destroyed != 0 {
		t.Fatalf("a protected worktree was captured or force-removed: calls=%v destroyed=%d", ws.calls, ws.destroyed)
	}
}
