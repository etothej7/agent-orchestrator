package integration

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/workspace/gitworktree"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/lifecycle"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	sessionmanager "github.com/aoagents/agent-orchestrator/backend/internal/session_manager"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// Worktree safety against real git: a replacement or shutdown removal never
// loses uncommitted work or a git-ignored secret, kill/teardown/cleanup refuse a
// dirty worktree, and Restore reuses a worktree that is still there.

type safetyHarness struct {
	t          *testing.T
	git        string
	repo       string
	backupRoot string
	manager    *sessionmanager.Manager
	store      *sqlite.Store
	runtime    *stubRuntime
}

func newSafetyHarness(t *testing.T) *safetyHarness {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not available")
	}
	ctx := context.Background()
	tmp := t.TempDir()
	repo := seedOriginClone(t, git, tmp)
	backupRoot := filepath.Join(tmp, "backups")
	workspace, err := gitworktree.New(gitworktree.Options{
		Binary: git, ManagedRoot: filepath.Join(tmp, "managed"), BackupRoot: backupRoot,
		RepoResolver: gitworktree.StaticRepoResolver{"mer": repo},
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := sqlitetest.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.UpsertProject(ctx, domain.ProjectRecord{
		ID: "mer", Path: repo, RegisteredAt: time.Now(),
		Config: domain.ProjectConfig{
			Worker:       domain.RoleOverride{Harness: domain.HarnessClaudeCode},
			Orchestrator: domain.RoleOverride{Harness: domain.HarnessClaudeCode},
		},
	}); err != nil {
		t.Fatal(err)
	}
	messenger := &captureMessenger{}
	lcm := lifecycle.New(store, messenger)
	rt := &stubRuntime{}
	manager := sessionmanager.New(sessionmanager.Deps{
		Runtime: rt, Agents: stubAgents{}, Workspace: workspace, Store: store,
		Messenger: messenger, Lifecycle: lcm,
		LookPath: func(string) (string, error) { return "/usr/bin/true", nil },
	})
	lcm.SetCompletionTerminator(manager)
	return &safetyHarness{t: t, git: git, repo: repo, backupRoot: backupRoot, manager: manager, store: store, runtime: rt}
}

func (h *safetyHarness) spawn() (domain.SessionID, string) {
	h.t.Helper()
	sess, _, _, err := h.manager.Spawn(context.Background(), ports.SpawnConfig{ProjectID: "mer", Kind: domain.KindWorker, Prompt: "do it"})
	if err != nil {
		h.t.Fatalf("spawn: %v", err)
	}
	rec, _, err := h.store.GetSession(context.Background(), sess.ID)
	if err != nil {
		h.t.Fatal(err)
	}
	return sess.ID, rec.Metadata.WorkspacePath
}

func (h *safetyHarness) dirty(worktree string) {
	h.t.Helper()
	for name, content := range map[string]string{
		"README.md":                 "edited by the agent\n",
		"agent-notes.md":            "untracked work\n",
		".gitignore":                ".env\nnode_modules/\n",
		".env":                      "TOKEN=dummy-secret\n",
		"node_modules/pkg/index.js": "x\n",
	} {
		full := filepath.Join(worktree, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			h.t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			h.t.Fatal(err)
		}
	}
}

func (h *safetyHarness) gitOut(args ...string) string {
	h.t.Helper()
	cmd := exec.Command(h.git, append([]string{"-C", h.repo}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		h.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestRetireForReplacementPreservesADirtyWorktreeBeforeRemovingIt(t *testing.T) {
	h := newSafetyHarness(t)
	ctx := context.Background()
	id, worktree := h.spawn()
	h.dirty(worktree)

	if err := h.manager.RetireForReplacement(ctx, id); err != nil {
		t.Fatalf("RetireForReplacement: %v", err)
	}
	rec, _, _ := h.store.GetSession(ctx, id)
	if !rec.IsTerminated {
		t.Fatal("retired session must be terminated")
	}
	if _, err := os.Stat(worktree); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worktree still on disk after a verified backup: %v", err)
	}
	// The uncommitted work is in a verified backup ref (and the earlier capture ref).
	refs := strings.Fields(h.gitOut("for-each-ref", "--format=%(refname)", "refs/ao-backup/"))
	if len(refs) == 0 {
		t.Fatal("no refs/ao-backup ref after retiring a dirty worktree")
	}
	files := h.gitOut("ls-tree", "-r", "--name-only", refs[0])
	for _, want := range []string{"README.md", "agent-notes.md"} {
		if !strings.Contains(files, want) {
			t.Fatalf("%s missing from the backup (%s)", want, files)
		}
	}
	for _, line := range strings.Split(files, "\n") {
		if line == ".env" || strings.HasPrefix(line, "node_modules/") {
			t.Fatalf("git-ignored %s was written into a git object", line)
		}
	}
	// The ignored secret went to the private archive, node_modules did not.
	var archived string
	_ = filepath.WalkDir(h.backupRoot, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == ".env" {
			archived = path
		}
		return nil
	})
	if archived == "" {
		t.Fatal("the ignored .env was not archived before removal")
	}
	if info, _ := os.Stat(archived); info.Mode().Perm() != 0o600 {
		t.Fatalf("archived .env mode = %v, want 0600", info.Mode().Perm())
	}
	if got, _ := os.ReadFile(archived); string(got) != "TOKEN=dummy-secret\n" {
		t.Fatalf("archived .env = %q", got)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(archived), "node_modules")); err == nil {
		t.Fatal("node_modules was archived")
	}
}

func TestKillAndCleanupRefuseADirtyWorktreeAndLeaveItOnDisk(t *testing.T) {
	h := newSafetyHarness(t)
	ctx := context.Background()
	id, worktree := h.spawn()
	h.dirty(worktree)

	freed, err := h.manager.Kill(ctx, id)
	if err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if freed {
		t.Fatal("a dirty worktree must not be freed by Kill")
	}
	rec, _, _ := h.store.GetSession(ctx, id)
	if !rec.IsTerminated {
		t.Fatal("Kill must still terminate the session")
	}
	for name, want := range map[string]string{"README.md": "edited by the agent\n", "agent-notes.md": "untracked work\n", ".env": "TOKEN=dummy-secret\n"} {
		if got, err := os.ReadFile(filepath.Join(worktree, name)); err != nil || string(got) != want {
			t.Fatalf("%s after a refused Kill = %q, %v", name, got, err)
		}
	}
	// Project teardown is Kill for every live session plus Cleanup: the dirty worktree is skipped, never forced.
	res, err := h.manager.Cleanup(ctx, "mer")
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if len(res.Skipped) != 1 || res.Skipped[0].SessionID != id {
		t.Fatalf("cleanup skipped = %+v, want the dirty session", res.Skipped)
	}
	if _, err := os.Stat(filepath.Join(worktree, "README.md")); err != nil {
		t.Fatalf("Cleanup removed a dirty worktree: %v", err)
	}
	if refs := h.gitOut("for-each-ref", "refs/ao-backup/"); refs != "" {
		t.Fatalf("a refused removal must not create backups: %s", refs)
	}
}

func TestKillRemovesACleanWorktree(t *testing.T) {
	h := newSafetyHarness(t)
	id, worktree := h.spawn()
	freed, err := h.manager.Kill(context.Background(), id)
	if err != nil || !freed {
		t.Fatalf("Kill clean worktree freed=%v err=%v", freed, err)
	}
	if _, err := os.Stat(worktree); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("clean worktree still on disk: %v", err)
	}
}

// Terminate then Restore: a worktree that survived the termination (dirty, so
// Kill left it) is reused as it is; one that is genuinely gone is recreated.
func TestRestoreReusesTheWorktreeKillLeftBehind(t *testing.T) {
	h := newSafetyHarness(t)
	ctx := context.Background()
	id, worktree := h.spawn()
	h.dirty(worktree)
	headBefore := strings.TrimSpace(runOut(t, h.git, worktree, "rev-parse", "HEAD"))
	if _, err := h.manager.Kill(ctx, id); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if _, err := h.manager.RestoreWithMode(ctx, id); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	rec, _, _ := h.store.GetSession(ctx, id)
	if rec.Metadata.WorkspacePath != worktree {
		t.Fatalf("restore moved the worktree: %q != %q", rec.Metadata.WorkspacePath, worktree)
	}
	if got := strings.TrimSpace(runOut(t, h.git, worktree, "rev-parse", "HEAD")); got != headBefore {
		t.Fatalf("HEAD changed on restore: %s != %s", got, headBefore)
	}
	for name, want := range map[string]string{"README.md": "edited by the agent\n", "agent-notes.md": "untracked work\n", ".env": "TOKEN=dummy-secret\n"} {
		if got, err := os.ReadFile(filepath.Join(worktree, name)); err != nil || string(got) != want {
			t.Fatalf("%s after Restore = %q, %v (a reused worktree keeps its contents)", name, got, err)
		}
	}
}

func TestRestoreRecreatesAWorktreeThatIsGenuinelyMissing(t *testing.T) {
	h := newSafetyHarness(t)
	ctx := context.Background()
	id, worktree := h.spawn()
	if _, err := h.manager.Kill(ctx, id); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if _, err := os.Stat(worktree); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("clean Kill should have removed the worktree: %v", err)
	}
	if _, err := h.manager.RestoreWithMode(ctx, id); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if _, err := os.Stat(filepath.Join(worktree, "README.md")); err != nil {
		t.Fatalf("worktree was not recreated: %v", err)
	}
}

func runOut(t *testing.T, git, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command(git, append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// Terminate then Restore of a CLEAN worktree: the removal still archives the
// git-ignored secrets (privately) before the worktree goes, and a failed archive
// refuses the removal instead of deleting them.
func (h *safetyHarness) cleanWithSecret(worktree string) {
	h.t.Helper()
	gitIn := func(args ...string) {
		out, err := exec.Command(h.git, append([]string{"-C", worktree, "-c", "user.email=ao@example.com", "-c", "user.name=Ao"}, args...)...).CombinedOutput()
		if err != nil {
			h.t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(worktree, ".gitignore"), []byte(".env\nnode_modules/\n"), 0o644); err != nil {
		h.t.Fatal(err)
	}
	gitIn("add", ".gitignore")
	gitIn("commit", "-m", "ignore env")
	if err := os.WriteFile(filepath.Join(worktree, ".env"), []byte("TOKEN=dummy-clean-secret\n"), 0o644); err != nil {
		h.t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(worktree, "node_modules", "pkg"), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "node_modules", "pkg", "index.js"), []byte("x\n"), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func TestKillOfACleanWorktreeArchivesItsIgnoredEnvFile(t *testing.T) {
	h := newSafetyHarness(t)
	id, worktree := h.spawn()
	h.cleanWithSecret(worktree)
	freed, err := h.manager.Kill(context.Background(), id)
	if err != nil || !freed {
		t.Fatalf("Kill freed=%v err=%v", freed, err)
	}
	if _, err := os.Stat(worktree); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("clean worktree still on disk: %v", err)
	}
	var archived string
	_ = filepath.WalkDir(h.backupRoot, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == ".env" {
			archived = path
		}
		return nil
	})
	if archived == "" {
		t.Fatal("the ignored .env was deleted without being archived")
	}
	if info, _ := os.Stat(archived); info.Mode().Perm() != 0o600 {
		t.Fatalf("archived .env mode = %v, want 0600", info.Mode().Perm())
	}
	if got, _ := os.ReadFile(archived); string(got) != "TOKEN=dummy-clean-secret\n" {
		t.Fatalf("archived .env = %q", got)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(archived), "node_modules")); err == nil {
		t.Fatal("node_modules was archived")
	}
}

func TestKillRefusesToRemoveACleanWorktreeWhoseSecretsCannotBeArchived(t *testing.T) {
	h := newSafetyHarness(t)
	id, worktree := h.spawn()
	h.cleanWithSecret(worktree)
	if err := os.WriteFile(h.backupRoot, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	freed, err := h.manager.Kill(context.Background(), id)
	if err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if freed {
		t.Fatal("the worktree was freed although its secrets could not be archived")
	}
	rec, _, _ := h.store.GetSession(context.Background(), id)
	if !rec.IsTerminated {
		t.Fatal("Kill must still terminate the session")
	}
	if got, readErr := os.ReadFile(filepath.Join(worktree, ".env")); readErr != nil || string(got) != "TOKEN=dummy-clean-secret\n" {
		t.Fatalf(".env after a refused Kill = %q, %v", got, readErr)
	}
}

// A worktree whose work cannot be backed up (here: a nested git repository) is
// refused BEFORE anything is stopped: the old orchestrator keeps running, its
// worktree is untouched, and the replacement fails cleanly instead of leaving a
// terminated session whose dirty worktree still holds the branch.
func TestRetireForReplacementRefusesBeforeTerminatingAnUnpreservableWorktree(t *testing.T) {
	h := newSafetyHarness(t)
	ctx := context.Background()
	id, worktree := h.spawn()
	nested := filepath.Join(worktree, "vendor", "tool")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init"}, {"-c", "user.email=ao@example.com", "-c", "user.name=Ao", "commit", "--allow-empty", "-m", "nested"}} {
		out, err := exec.Command(h.git, append([]string{"-C", nested}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(nested, "work.txt"), []byte("nested work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	destroyedBefore := h.runtime.destroyed

	for name, call := range map[string]func() error{
		"preflight": func() error { return h.manager.PreflightRetireForReplacement(ctx, id) },
		"retire":    func() error { return h.manager.RetireForReplacement(ctx, id) },
	} {
		err := call()
		if !errors.Is(err, ports.ErrWorkspaceDirty) || !strings.Contains(err.Error(), worktree) || !strings.Contains(err.Error(), "nested git repository") {
			t.Fatalf("%s error = %v, want a refusal naming the worktree and the nested repository", name, err)
		}
	}
	rec, _, _ := h.store.GetSession(ctx, id)
	if rec.IsTerminated {
		t.Fatal("a refused retirement must leave the session running")
	}
	if h.runtime.destroyed != destroyedBefore {
		t.Fatal("the runtime was destroyed although the retirement was refused")
	}
	if got, err := os.ReadFile(filepath.Join(nested, "work.txt")); err != nil || string(got) != "nested work\n" {
		t.Fatalf("worktree content changed: %v %q", err, got)
	}
	if _, err := os.Stat(worktree + ".kept"); err == nil {
		t.Fatal("a refused retirement must not move the worktree")
	}
	if listing := h.gitOut("worktree", "list", "--porcelain"); !strings.Contains(listing, "feature/") && !strings.Contains(listing, worktree) {
		t.Fatalf("worktree no longer registered: %s", listing)
	}
}

// The preflight passes, then something changes before the removal (here the agent's
// runtime teardown leaves a nested git repository behind). RetireForReplacement must
// still succeed: the worktree is kept aside whole, the registration released, and the
// replacement can claim the branch.
func TestRetireForReplacementKeepsTheWorktreeAsideWhenItBecomesUnpreservableLate(t *testing.T) {
	h := newSafetyHarness(t)
	ctx := context.Background()
	id, worktree := h.spawn()
	rec, _, _ := h.store.GetSession(ctx, id)
	branch := rec.Metadata.Branch
	h.runtime.onDestroy = func() {
		nested := filepath.Join(worktree, "vendor", "tool")
		if err := os.MkdirAll(nested, 0o755); err != nil {
			h.t.Fatal(err)
		}
		for _, args := range [][]string{{"init"}, {"-c", "user.email=ao@example.com", "-c", "user.name=Ao", "commit", "--allow-empty", "-m", "nested"}} {
			if out, err := exec.Command(h.git, append([]string{"-C", nested}, args...)...).CombinedOutput(); err != nil {
				h.t.Fatalf("git %v: %v\n%s", args, err, out)
			}
		}
		if err := os.WriteFile(filepath.Join(nested, "work.txt"), []byte("late nested work\n"), 0o644); err != nil {
			h.t.Fatal(err)
		}
	}
	if err := h.manager.PreflightRetireForReplacement(ctx, id); err != nil {
		t.Fatalf("preflight must pass before the late change: %v", err)
	}
	if err := h.manager.RetireForReplacement(ctx, id); err != nil {
		t.Fatalf("RetireForReplacement must succeed by keeping the worktree aside: %v", err)
	}
	rec, _, _ = h.store.GetSession(ctx, id)
	if !rec.IsTerminated {
		t.Fatal("the retired session must be terminated")
	}
	if _, err := os.Stat(worktree); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("original worktree path should be free: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(worktree+".kept", "vendor", "tool", "work.txt")); err != nil || string(got) != "late nested work\n" {
		t.Fatalf("kept worktree lost its content: %v %q", err, got)
	}
	// The replacement can claim the same branch.
	replacement := filepath.Join(filepath.Dir(worktree), "replacement")
	if out, err := exec.Command(h.git, "-C", h.repo, "worktree", "add", replacement, branch).CombinedOutput(); err != nil {
		t.Fatalf("the branch %s is still held: %v\n%s", branch, err, out)
	}
}
