package gitworktree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// These tests pin the worktree-safety contract: ForceDestroy never loses
// uncommitted work, a detached head, or a git-ignored secret, and refuses when it
// cannot prove it saved them; normal Destroy still refuses a dirty worktree;
// Restore reuses a worktree that exists and recreates one that is missing.

type removalFixture struct {
	t          *testing.T
	git        string
	ws         *Workspace
	repo       string
	info       ports.WorkspaceInfo
	backupRoot string
	logs       *bytes.Buffer
}

func newRemovalFixture(t *testing.T, session string) *removalFixture {
	t.Helper()
	git := requireGit(t)
	tmp := t.TempDir()
	repo := setupOriginClone(t, git, tmp)
	logs := &bytes.Buffer{}
	backupRoot := filepath.Join(tmp, "backups")
	ws, err := New(Options{
		Binary: git, ManagedRoot: filepath.Join(tmp, "managed"), BackupRoot: backupRoot,
		RepoResolver: StaticRepoResolver{"proj": repo},
		Logger:       slog.New(slog.NewTextHandler(logs, nil)),
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	info, err := ws.Create(context.Background(), ports.WorkspaceConfig{ProjectID: "proj", SessionID: domain.SessionID(session), Branch: "feature/" + session})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return &removalFixture{t: t, git: git, ws: ws, repo: repo, info: info, backupRoot: backupRoot, logs: logs}
}

func (f *removalFixture) write(name, content string) {
	f.t.Helper()
	full := filepath.Join(f.info.Path, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		f.t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		f.t.Fatalf("write %s: %v", name, err)
	}
}

func (f *removalFixture) backupRefs() []string {
	f.t.Helper()
	out := gitOutput(f.t, f.git, f.repo, "for-each-ref", "--format=%(refname)", backupRefPrefix)
	if out == "" {
		return nil
	}
	return strings.Fields(out)
}

func (f *removalFixture) show(ref, name string) string {
	f.t.Helper()
	return gitOutput(f.t, f.git, f.repo, "show", ref+":"+name)
}

func (f *removalFixture) inTree(ref, name string) bool {
	f.t.Helper()
	for _, n := range strings.Split(gitOutput(f.t, f.git, f.repo, "ls-tree", "-r", "--name-only", ref), "\n") {
		if n == name {
			return true
		}
	}
	return false
}

func (f *removalFixture) removed() bool {
	_, err := os.Stat(f.info.Path)
	return errors.Is(err, os.ErrNotExist)
}

func (f *removalFixture) force() error {
	err := f.ws.ForceDestroy(context.Background(), f.info)
	f.ws.waitForDiscards()
	return err
}

func TestForceDestroyPreservesModifiedStagedAndUntrackedWork(t *testing.T) {
	f := newRemovalFixture(t, "sess-dirty")
	f.write("README.md", "edited by the agent\n")                // modified tracked
	f.write("staged.txt", "staged new file\n")                   // staged
	runGit(t, f.git, f.info.Path, "add", "staged.txt")           //
	f.write("notes/untracked.txt", "untracked work\n")           // untracked, in a new directory
	f.write("node_modules/pkg/index.js", "module.exports = 1\n") // ignored build output
	f.write(".gitignore", "node_modules/\n")                     // makes node_modules ignored
	if err := f.force(); err != nil {
		t.Fatalf("ForceDestroy: %v", err)
	}
	if !f.removed() {
		t.Fatal("worktree was not removed after a verified backup")
	}
	refs := f.backupRefs()
	if len(refs) != 1 || !strings.HasPrefix(refs[0], backupRefPrefix+"sess-dirty/") {
		t.Fatalf("backup refs = %v, want one under %ssess-dirty/", refs, backupRefPrefix)
	}
	ref := refs[0]
	if got := f.show(ref, "README.md"); got != "edited by the agent" {
		t.Fatalf("modified file in backup = %q", got)
	}
	for _, name := range []string{"staged.txt", "notes/untracked.txt", ".gitignore"} {
		if !f.inTree(ref, name) {
			t.Fatalf("%s missing from the backup", name)
		}
	}
	if f.inTree(ref, "node_modules/pkg/index.js") {
		t.Fatal("ignored build output was written into the backup")
	}
	if !strings.Contains(f.logs.String(), ref) {
		t.Fatalf("preservation log does not say where the work went: %s", f.logs.String())
	}
}

func TestForceDestroyPreservesEditsMadeAfterAnEarlierCapture(t *testing.T) {
	f := newRemovalFixture(t, "sess-late")
	f.write("early.txt", "captured first\n")
	if _, err := f.ws.StashUncommitted(context.Background(), f.info); err != nil {
		t.Fatalf("StashUncommitted: %v", err)
	}
	// The agent is still running between the caller's capture and the removal.
	f.write("late.txt", "written after the capture\n")
	if err := f.force(); err != nil {
		t.Fatalf("ForceDestroy: %v", err)
	}
	refs := f.backupRefs()
	if len(refs) != 1 || !f.inTree(refs[0], "late.txt") || !f.inTree(refs[0], "early.txt") {
		t.Fatalf("late edit lost: refs=%v", refs)
	}
}

func TestForceDestroyKeepsStashEntriesAndUnpushedCommits(t *testing.T) {
	f := newRemovalFixture(t, "sess-commits")
	// An unpushed commit.
	f.write("feature.go", "package feature\n")
	runGit(t, f.git, f.info.Path, "add", "feature.go")
	runGit(t, f.git, f.info.Path, "-c", "user.email=ao@example.com", "-c", "user.name=Ao", "commit", "-m", "unpushed work")
	commit := gitOutput(t, f.git, f.info.Path, "rev-parse", "HEAD")
	// A stash entry on the branch.
	f.write("stashed.txt", "stashed\n")
	runGit(t, f.git, f.info.Path, "add", "stashed.txt")
	runGit(t, f.git, f.info.Path, "-c", "user.email=ao@example.com", "-c", "user.name=Ao", "stash", "push", "-m", "keep me")
	if err := f.force(); err != nil {
		t.Fatalf("ForceDestroy: %v", err)
	}
	if !f.removed() {
		t.Fatal("clean worktree with only commits and a stash should be removed")
	}
	// The commit is still on the branch and the stash entry is still in the repository.
	if got := gitOutput(t, f.git, f.repo, "rev-parse", "refs/heads/feature/sess-commits"); got != commit {
		t.Fatalf("branch moved: %s != %s", got, commit)
	}
	if n := gitOutput(t, f.git, f.repo, "rev-list", "--count", "refs/heads/feature/sess-commits", "--not", "--remotes"); n != "1" {
		t.Fatalf("unpushed commits on branch = %s, want 1", n)
	}
	if !strings.Contains(gitOutput(t, f.git, f.repo, "stash", "list"), "keep me") {
		t.Fatal("stash entry lost with the worktree")
	}
	log := f.logs.String()
	if !strings.Contains(log, "unpushedCommitsOnBranch=1") || !strings.Contains(log, "stashEntriesOnBranch=1") {
		t.Fatalf("log does not report the commits and stash that stay on the branch: %s", log)
	}
	if len(f.backupRefs()) != 0 {
		t.Fatalf("a clean worktree must not create a backup ref: %v", f.backupRefs())
	}
}

func TestForceDestroyOfACleanWorktreeCreatesNoBackup(t *testing.T) {
	f := newRemovalFixture(t, "sess-clean")
	if err := f.force(); err != nil {
		t.Fatalf("ForceDestroy: %v", err)
	}
	if !f.removed() {
		t.Fatal("clean worktree was not removed")
	}
	if refs := f.backupRefs(); len(refs) != 0 {
		t.Fatalf("clean worktree produced backup refs %v", refs)
	}
	if _, err := os.Stat(f.backupRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("clean worktree created a secret archive: %v", err)
	}
}

func TestForceDestroyArchivesIgnoredSecretsPrivatelyAndNeverInGit(t *testing.T) {
	f := newRemovalFixture(t, "sess-secret")
	f.write(".gitignore", ".env\n.env.*\n*.local\nnode_modules/\n")
	f.write(".env", "TOKEN=dummy-secret-value\n")
	f.write(".env.production", "DB=dummy-prod\n")
	f.write("apps/web/settings.local", "local=dummy\n")
	f.write("node_modules/pkg/.env", "SHOULD_NOT_BE_ARCHIVED=1\n")
	f.write("node_modules/pkg/index.js", "x\n")
	f.write("work.txt", "dirty tracked-by-backup work\n") // makes the worktree dirty too
	if err := f.force(); err != nil {
		t.Fatalf("ForceDestroy: %v", err)
	}
	// Archived: private directory layout, 0600 files, contents intact.
	sessionDir := filepath.Join(f.backupRoot, "sess-secret")
	stamps, err := os.ReadDir(sessionDir)
	if err != nil || len(stamps) != 1 {
		t.Fatalf("archive directories = %v, %v", stamps, err)
	}
	archive := filepath.Join(sessionDir, stamps[0].Name())
	for _, dir := range []string{sessionDir, archive, filepath.Join(archive, "files"), filepath.Join(archive, "files", "apps", "web")} {
		info, err := os.Stat(dir)
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("%s mode = %v err=%v, want 0700", dir, info.Mode().Perm(), err)
		}
	}
	for name, want := range map[string]string{".env": "TOKEN=dummy-secret-value\n", ".env.production": "DB=dummy-prod\n", "apps/web/settings.local": "local=dummy\n"} {
		path := filepath.Join(archive, "files", name)
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %v err=%v, want 0600", name, info.Mode().Perm(), err)
		}
		if got, _ := os.ReadFile(path); string(got) != want {
			t.Fatalf("%s archived as %q", name, got)
		}
	}
	// node_modules is not archived.
	if _, err := os.Stat(filepath.Join(archive, "files", "node_modules")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("node_modules was archived: %v", err)
	}
	// Secrets are never written into git: the backup commit holds the dirty work only.
	refs := f.backupRefs()
	if len(refs) != 1 || !f.inTree(refs[0], "work.txt") {
		t.Fatalf("backup refs = %v", refs)
	}
	for _, name := range []string{".env", ".env.production", "apps/web/settings.local", "node_modules/pkg/.env"} {
		if f.inTree(refs[0], name) {
			t.Fatalf("secret %s was written into a git object", name)
		}
	}
	// The log carries names and a count, never contents.
	log := f.logs.String()
	if strings.Contains(log, "dummy") || strings.Contains(log, "SHOULD_NOT_BE_ARCHIVED") {
		t.Fatalf("log leaked a secret value: %s", log)
	}
	if !strings.Contains(log, "secretFileCount=3") || !strings.Contains(log, ".env.production") {
		t.Fatalf("log does not name the archived files: %s", log)
	}
}

// keptAside asserts the worktree directory was kept whole beside its original
// path (and the registration released), returning where it went.
func (f *removalFixture) keptAside(suffix string) string {
	f.t.Helper()
	if _, err := os.Stat(f.info.Path); !errors.Is(err, os.ErrNotExist) {
		f.t.Fatalf("the original path should be free after keeping the worktree aside: %v", err)
	}
	kept := f.info.Path + suffix
	if _, err := os.Stat(kept); err != nil {
		f.t.Fatalf("worktree directory was not kept at %s: %v", kept, err)
	}
	records, err := f.ws.listRecords(context.Background(), f.repo)
	if err != nil {
		f.t.Fatalf("list: %v", err)
	}
	if _, ok := findWorktree(records, f.info.Path); ok {
		f.t.Fatal("kept-aside worktree is still registered, so it would keep holding its branch")
	}
	return kept
}

// A worktree whose secrets cannot be archived is refused up front by the
// preflight; if the removal happens anyway (a race), ForceDestroy keeps the
// directory aside instead of deleting it, and the branch is released.
func TestForceDestroyKeepsTheWorktreeAsideWhenTheSecretArchiveFails(t *testing.T) {
	f := newRemovalFixture(t, "sess-archive-fail")
	f.write(".gitignore", ".env\n")
	f.write(".env", "TOKEN=dummy\n")
	// A regular file where the backup root must be a directory.
	if err := os.WriteFile(f.backupRoot, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("block backup root: %v", err)
	}
	preflight := f.ws.PreflightRemoval(context.Background(), f.info)
	if !errors.Is(preflight, ports.ErrWorkspaceDirty) || !strings.Contains(preflight.Error(), f.info.Path) || !strings.Contains(preflight.Error(), filepath.Join(f.backupRoot, "sess-archive-fail")) {
		t.Fatalf("PreflightRemoval = %v, want a refusal naming the worktree and the archive location", preflight)
	}
	if err := f.force(); err != nil {
		t.Fatalf("ForceDestroy must keep the directory aside, not fail: %v", err)
	}
	kept := f.keptAside(".kept")
	if got, _ := os.ReadFile(filepath.Join(kept, ".env")); string(got) != "TOKEN=dummy\n" {
		t.Fatalf("kept .env = %q", got)
	}
	if !strings.Contains(f.logs.String(), "kept aside instead of being deleted") {
		t.Fatalf("no warning says where the worktree went: %s", f.logs.String())
	}
	// The branch is free again: a new worktree can claim it.
	runGit(t, f.git, f.repo, "worktree", "add", filepath.Join(filepath.Dir(f.info.Path), "again"), "feature/sess-archive-fail")
}

func TestForceDestroyKeepsTheWorktreeAsideWhenTheBackupCannotBeVerified(t *testing.T) {
	f := newRemovalFixture(t, "sess-verify-fail")
	f.write(".gitignore", ".env\n")
	f.write(".env", "TOKEN=dummy\n")
	f.write("important.txt", "do not lose\n")
	inner := f.ws.run
	f.ws.run = func(ctx context.Context, binary string, args ...string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "ls-tree") {
			return nil, errors.New("simulated ls-tree failure")
		}
		return inner(ctx, binary, args...)
	}
	if err := f.ws.ForceDestroy(context.Background(), f.info); err != nil {
		t.Fatalf("ForceDestroy must keep the directory aside, not fail: %v", err)
	}
	kept := f.keptAside(".kept")
	if got, _ := os.ReadFile(filepath.Join(kept, "important.txt")); string(got) != "do not lose\n" {
		t.Fatalf("kept file lost: %q", got)
	}
	// The secret archive made on the way is dropped: the secrets are still in the kept directory.
	if entries, _ := os.ReadDir(filepath.Join(f.backupRoot, "sess-verify-fail")); len(entries) != 0 {
		t.Fatalf("redundant secret archive left behind: %v", entries)
	}
}

func TestPreflightAndForceDestroyHandleANestedGitRepository(t *testing.T) {
	f := newRemovalFixture(t, "sess-nested")
	nested := filepath.Join(f.info.Path, "vendor", "tool")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	runGit(t, f.git, nested, "init")
	if err := os.WriteFile(filepath.Join(nested, "a.txt"), []byte("nested work\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	runGit(t, f.git, nested, "add", "a.txt")
	runGit(t, f.git, nested, "-c", "user.email=ao@example.com", "-c", "user.name=Ao", "commit", "-m", "nested")
	err := f.ws.PreflightRemoval(context.Background(), f.info)
	if !errors.Is(err, ports.ErrWorkspaceDirty) || !strings.Contains(err.Error(), "nested git repository") {
		t.Fatalf("PreflightRemoval = %v, want a refusal that names the nested repository", err)
	}
	if err := f.force(); err != nil {
		t.Fatalf("ForceDestroy: %v", err)
	}
	kept := f.keptAside(".kept")
	if got, _ := os.ReadFile(filepath.Join(kept, "vendor", "tool", "a.txt")); string(got) != "nested work\n" {
		t.Fatalf("nested repository content lost: %q", got)
	}
}

func TestPreflightAndForceDestroyBoundTheBackupSize(t *testing.T) {
	f := newRemovalFixture(t, "sess-big")
	old := backupMaxBytes
	backupMaxBytes = 16
	t.Cleanup(func() { backupMaxBytes = old })
	f.write("big.bin", strings.Repeat("x", 1024))
	err := f.ws.PreflightRemoval(context.Background(), f.info)
	if !errors.Is(err, ports.ErrWorkspaceDirty) || !strings.Contains(err.Error(), "byte limit") {
		t.Fatalf("PreflightRemoval = %v, want a size refusal", err)
	}
	if err := f.force(); err != nil {
		t.Fatalf("ForceDestroy: %v", err)
	}
	kept := f.keptAside(".kept")
	if info, err := os.Stat(filepath.Join(kept, "big.bin")); err != nil || info.Size() != 1024 {
		t.Fatalf("oversized file not kept: %v", err)
	}
	if refs := f.backupRefs(); len(refs) != 0 {
		t.Fatalf("an oversized worktree must not be copied into the repository: %v", refs)
	}
}

func TestForceDestroyKeepsTheWorktreeAsideWhenPreservationRunsOutOfTime(t *testing.T) {
	f := newRemovalFixture(t, "sess-slow")
	f.write("work.txt", "x\n")
	old := preserveBudget
	preserveBudget = 100 * time.Millisecond
	t.Cleanup(func() { preserveBudget = old })
	inner := f.ws.run
	f.ws.run = func(ctx context.Context, binary string, args ...string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "--porcelain=v1") {
			<-ctx.Done() // a walk that never finishes
			return nil, ctx.Err()
		}
		return inner(ctx, binary, args...)
	}
	if err := f.ws.ForceDestroy(context.Background(), f.info); err != nil {
		t.Fatalf("ForceDestroy: %v", err)
	}
	kept := f.keptAside(".kept")
	if got, _ := os.ReadFile(filepath.Join(kept, "work.txt")); string(got) != "x\n" {
		t.Fatalf("work lost on timeout: %q", got)
	}
}

// If putting a discarded directory back fails, it must not be left where the next
// start's sweep deletes it: it is kept beside the original path, and as a last
// resort marked so the sweep skips it.
func TestFailedUndiscardKeepsTheDataOutOfTheBootSweep(t *testing.T) {
	newWS := func(t *testing.T) (*Workspace, string, string) {
		tmp := t.TempDir()
		ws, err := New(Options{ManagedRoot: filepath.Join(tmp, "managed"), RepoResolver: StaticRepoResolver{"proj": tmp}})
		if err != nil {
			t.Fatalf("new: %v", err)
		}
		path := filepath.Join(ws.managedRoot, "proj", "sess")
		discarded := filepath.Join(ws.discardedRoot(), "sess-1-0")
		if err := os.MkdirAll(discarded, 0o755); err != nil {
			t.Fatalf("mkdir discarded: %v", err)
		}
		if err := os.WriteFile(filepath.Join(discarded, "work.txt"), []byte("unpreserved work"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		// The original path was re-created meanwhile, so the rename back fails.
		if err := mkdirFile(path, "other.txt"); err != nil {
			t.Fatalf("occupy path: %v", err)
		}
		return ws, path, discarded
	}
	t.Run("kept beside the original path", func(t *testing.T) {
		ws, path, discarded := newWS(t)
		ws.undiscard(discarded, path)
		if got, err := os.ReadFile(filepath.Join(path+".kept", "work.txt")); err != nil || string(got) != "unpreserved work" {
			t.Fatalf("data not kept beside the path: %v %q", err, got)
		}
		ws.sweepDiscarded()
		ws.waitForDiscards()
		if _, err := os.Stat(filepath.Join(path+".kept", "work.txt")); err != nil {
			t.Fatalf("sweep deleted kept data: %v", err)
		}
	})
	t.Run("marked when it cannot be moved either", func(t *testing.T) {
		ws, path, discarded := newWS(t)
		// Occupy every candidate destination so keeping aside is impossible too.
		if err := os.MkdirAll(path+".kept", 0o755); err != nil {
			t.Fatal(err)
		}
		for i := 2; i <= 100; i++ {
			if err := os.MkdirAll(fmt.Sprintf("%s.kept-%d", path, i), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		ws.undiscard(discarded, path)
		if _, err := os.Stat(filepath.Join(discarded, keepMarker)); err != nil {
			t.Fatalf("no keep marker written: %v", err)
		}
		// A simulated next start: construction sweeps the discard root.
		if _, err := New(Options{ManagedRoot: ws.managedRoot, RepoResolver: StaticRepoResolver{"proj": ws.managedRoot}}); err != nil {
			t.Fatalf("new (boot): %v", err)
		}
		ws.waitForDiscards()
		if got, err := os.ReadFile(filepath.Join(discarded, "work.txt")); err != nil || string(got) != "unpreserved work" {
			t.Fatalf("the boot sweep deleted unpreserved work: %v %q", err, got)
		}
		// Control: an unmarked leftover is still swept.
		leftover := filepath.Join(ws.discardedRoot(), "stale-9-0")
		if err := os.MkdirAll(leftover, 0o755); err != nil {
			t.Fatal(err)
		}
		ws.sweepDiscarded()
		ws.waitForDiscards()
		if _, err := os.Stat(leftover); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("an unmarked leftover must still be swept: %v", err)
		}
	})
}

func TestForceDestroyPinsADetachedHeadNoRefReaches(t *testing.T) {
	f := newRemovalFixture(t, "sess-detached")
	runGit(t, f.git, f.info.Path, "checkout", "--detach")
	f.write("orphan.txt", "only reachable from this detached HEAD\n")
	runGit(t, f.git, f.info.Path, "add", "orphan.txt")
	runGit(t, f.git, f.info.Path, "-c", "user.email=ao@example.com", "-c", "user.name=Ao", "commit", "-m", "orphan commit")
	head := gitOutput(t, f.git, f.info.Path, "rev-parse", "HEAD")
	if err := f.force(); err != nil {
		t.Fatalf("ForceDestroy: %v", err)
	}
	refs := f.backupRefs()
	if len(refs) != 1 || gitOutput(t, f.git, f.repo, "rev-parse", refs[0]) != head {
		t.Fatalf("detached HEAD %s not pinned by a backup ref: %v", head, refs)
	}
	if !strings.Contains(f.logs.String(), "pinnedDetachedHead=true") {
		t.Fatalf("log does not report the pin: %s", f.logs.String())
	}
}

func TestForceDestroyNeverDeletesAnUnregisteredDirectory(t *testing.T) {
	f := newRemovalFixture(t, "sess-stray")
	// Unregister the worktree but leave the directory (and a file in it) behind.
	runGit(t, f.git, f.repo, "worktree", "prune")
	stray := filepath.Join(filepath.Dir(f.info.Path), "stray-dir")
	if err := os.MkdirAll(stray, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stray, "valuable.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	info := f.info
	info.Path = stray
	if err := f.ws.ForceDestroy(context.Background(), info); err != nil {
		t.Fatalf("ForceDestroy: %v", err)
	}
	f.ws.waitForDiscards()
	if _, err := os.Stat(stray); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stray path still occupies the worktree path: %v", err)
	}
	if got, err := os.ReadFile(stray + ".stray/valuable.txt"); err != nil || string(got) != "keep" {
		t.Fatalf("unregistered directory was not preserved aside: %v %q", err, got)
	}
}

func TestPreserveForRemovalReusesAnIdenticalBackup(t *testing.T) {
	f := newRemovalFixture(t, "sess-reuse")
	f.write("once.txt", "same content\n")
	ctx := context.Background()
	first, err := f.ws.preserveForRemoval(ctx, f.repo, f.info.Path, f.info.Path, f.info)
	if err != nil {
		t.Fatalf("first preserve: %v", err)
	}
	second, err := f.ws.preserveForRemoval(ctx, f.repo, f.info.Path, f.info.Path, f.info)
	if err != nil {
		t.Fatalf("second preserve: %v", err)
	}
	if first.ref == "" || first.ref != second.ref || len(f.backupRefs()) != 1 {
		t.Fatalf("identical state spawned extra refs: %q %q %v", first.ref, second.ref, f.backupRefs())
	}
}

// Kill, project teardown and cleanup use Destroy, which keeps refusing a dirty
// worktree and leaves it on disk; a worktree with only ignored files, commits
// or stash entries is still removable.
func TestDestroyRefusesEveryKindOfDirtyWorktreeAndRemovesCleanOnes(t *testing.T) {
	for name, dirty := range map[string]func(f *removalFixture){
		"modified":  func(f *removalFixture) { f.write("README.md", "changed\n") },
		"untracked": func(f *removalFixture) { f.write("new.txt", "new\n") },
		"staged": func(f *removalFixture) {
			f.write("staged.txt", "s\n")
			runGit(f.t, f.git, f.info.Path, "add", "staged.txt")
		},
	} {
		t.Run("dirty/"+name, func(t *testing.T) {
			f := newRemovalFixture(t, "sess-destroy-"+name)
			dirty(f)
			err := f.ws.Destroy(context.Background(), f.info)
			if !errors.Is(err, ports.ErrWorkspaceDirty) {
				t.Fatalf("Destroy error = %v, want ErrWorkspaceDirty", err)
			}
			if _, statErr := os.Stat(f.info.Path); statErr != nil {
				t.Fatalf("dirty worktree was removed: %v", statErr)
			}
		})
	}
	t.Run("clean", func(t *testing.T) {
		f := newRemovalFixture(t, "sess-destroy-clean")
		f.write(".gitignore", "build/\n")
		runGit(t, f.git, f.info.Path, "add", ".gitignore")
		runGit(t, f.git, f.info.Path, "-c", "user.email=ao@example.com", "-c", "user.name=Ao", "commit", "-m", "ignore build")
		f.write("build/out.bin", "ignored build output\n")
		if err := f.ws.Destroy(context.Background(), f.info); err != nil {
			t.Fatalf("Destroy of a clean worktree: %v", err)
		}
		f.ws.waitForDiscards()
		if !f.removed() {
			t.Fatal("clean worktree was not removed")
		}
	})
}

func TestRestoreReusesAnExistingWorktreeAsItIs(t *testing.T) {
	f := newRemovalFixture(t, "sess-reuse-restore")
	f.write("committed.txt", "committed\n")
	runGit(t, f.git, f.info.Path, "add", "committed.txt")
	runGit(t, f.git, f.info.Path, "-c", "user.email=ao@example.com", "-c", "user.name=Ao", "commit", "-m", "work")
	f.write("README.md", "dirty edit in place\n")
	f.write("scratch/untracked.txt", "untracked\n")
	headBefore := gitOutput(t, f.git, f.info.Path, "rev-parse", "HEAD")
	restored, err := f.ws.Restore(context.Background(), ports.WorkspaceConfig{ProjectID: "proj", SessionID: f.info.SessionID, Branch: f.info.Branch, Path: f.info.Path})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if restored.Path != f.info.Path || restored.Branch != f.info.Branch {
		t.Fatalf("restore moved the worktree: %+v vs %+v", restored, f.info)
	}
	if got := gitOutput(t, f.git, f.info.Path, "rev-parse", "HEAD"); got != headBefore {
		t.Fatalf("HEAD changed on restore: %s != %s", got, headBefore)
	}
	for name, want := range map[string]string{"README.md": "dirty edit in place\n", "scratch/untracked.txt": "untracked\n", "committed.txt": "committed\n"} {
		if got, _ := os.ReadFile(filepath.Join(f.info.Path, name)); string(got) != want {
			t.Fatalf("%s = %q after restore, want it untouched", name, got)
		}
	}
}

func TestRestoreRecreatesAMissingWorktreeOnItsBranch(t *testing.T) {
	f := newRemovalFixture(t, "sess-missing")
	f.write("committed.txt", "committed\n")
	runGit(t, f.git, f.info.Path, "add", "committed.txt")
	runGit(t, f.git, f.info.Path, "-c", "user.email=ao@example.com", "-c", "user.name=Ao", "commit", "-m", "work")
	head := gitOutput(t, f.git, f.info.Path, "rev-parse", "HEAD")
	if err := os.RemoveAll(f.info.Path); err != nil {
		t.Fatalf("remove worktree directory: %v", err)
	}
	restored, err := f.ws.Restore(context.Background(), ports.WorkspaceConfig{ProjectID: "proj", SessionID: f.info.SessionID, Branch: f.info.Branch, Path: f.info.Path})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got := gitOutput(t, f.git, restored.Path, "rev-parse", "HEAD"); got != head {
		t.Fatalf("recreated worktree is not on the branch tip: %s != %s", got, head)
	}
	if got, _ := os.ReadFile(filepath.Join(restored.Path, "committed.txt")); string(got) != "committed\n" {
		t.Fatalf("committed work missing after recreate: %q", got)
	}
}

// A clean worktree (Terminate, then Restore) still carries git-ignored files that
// go with it: local secrets are archived first, caches and build output are not.
func (f *removalFixture) cleanWithIgnoredFiles() {
	f.t.Helper()
	f.write(".gitignore", ".env\n.env.*\nnode_modules/\nbuild/\n")
	runGit(f.t, f.git, f.info.Path, "add", ".gitignore")
	runGit(f.t, f.git, f.info.Path, "-c", "user.email=ao@example.com", "-c", "user.name=Ao", "commit", "-m", "ignore secrets and build output")
	f.write(".env", "TOKEN=dummy-clean-secret\n")
	f.write(".env.local", "LOCAL=dummy\n")
	f.write("node_modules/pkg/index.js", "x\n")
	f.write("build/out.bin", "ignored build output\n")
}

func (f *removalFixture) archivedFile(name string) string {
	f.t.Helper()
	var found string
	_ = filepath.WalkDir(f.backupRoot, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(path, string(filepath.Separator)+filepath.Join("files", name)) {
			found = path
		}
		return nil
	})
	return found
}

func TestDestroyOfACleanWorktreeArchivesItsIgnoredSecrets(t *testing.T) {
	for name, blockDiscard := range map[string]bool{"fast path": false, "git-driven path": true} {
		t.Run(name, func(t *testing.T) {
			f := newRemovalFixture(t, "sess-destroy-secret")
			f.cleanWithIgnoredFiles()
			if blockDiscard {
				// Without a usable discard root the rename-aside is impossible and
				// Destroy falls back to `git worktree remove`.
				if err := os.WriteFile(f.ws.discardedRoot(), []byte("not a directory"), 0o600); err != nil {
					t.Fatalf("block discard root: %v", err)
				}
			}
			if err := f.ws.Destroy(context.Background(), f.info); err != nil {
				t.Fatalf("Destroy of a clean worktree: %v", err)
			}
			f.ws.waitForDiscards()
			if !f.removed() {
				t.Fatal("clean worktree was not removed")
			}
			for file, want := range map[string]string{".env": "TOKEN=dummy-clean-secret\n", ".env.local": "LOCAL=dummy\n"} {
				path := f.archivedFile(file)
				if path == "" {
					t.Fatalf("%s was not archived before the removal", file)
				}
				info, err := os.Stat(path)
				if err != nil || info.Mode().Perm() != 0o600 {
					t.Fatalf("%s archived with mode %v err=%v, want 0600", file, info.Mode().Perm(), err)
				}
				if got, _ := os.ReadFile(path); string(got) != want {
					t.Fatalf("%s archived as %q", file, got)
				}
			}
			if f.archivedFile("node_modules/pkg/index.js") != "" || f.archivedFile("build/out.bin") != "" {
				t.Fatal("caches or build output were archived")
			}
			if log := f.logs.String(); strings.Contains(log, "dummy") || !strings.Contains(log, "secretFileCount=2") {
				t.Fatalf("log must name the files and never their contents: %s", log)
			}
		})
	}
}

func TestDestroyRefusesACleanWorktreeWhenTheSecretArchiveFails(t *testing.T) {
	f := newRemovalFixture(t, "sess-destroy-archive-fail")
	f.cleanWithIgnoredFiles()
	if err := os.WriteFile(f.backupRoot, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("block backup root: %v", err)
	}
	err := f.ws.Destroy(context.Background(), f.info)
	if !errors.Is(err, ports.ErrWorkspaceDirty) {
		t.Fatalf("Destroy error = %v, want a refusal wrapping ErrWorkspaceDirty", err)
	}
	for _, want := range []string{f.info.Path, "git-ignored secret files", filepath.Join(f.backupRoot, "sess-destroy-archive-fail")} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal %q does not say %q", err, want)
		}
	}
	if got, readErr := os.ReadFile(filepath.Join(f.info.Path, ".env")); readErr != nil || string(got) != "TOKEN=dummy-clean-secret\n" {
		t.Fatalf("refused Destroy changed the worktree: %v %q", readErr, got)
	}
	records, listErr := f.ws.listRecords(context.Background(), f.repo)
	if listErr != nil {
		t.Fatalf("list: %v", listErr)
	}
	if _, ok := findWorktree(records, f.info.Path); !ok {
		t.Fatal("refused Destroy left the worktree unregistered")
	}
}

func TestDestroyOfACleanWorktreeWithoutSecretsCreatesNoArchive(t *testing.T) {
	f := newRemovalFixture(t, "sess-destroy-nosecret")
	f.write(".gitignore", "build/\n")
	runGit(t, f.git, f.info.Path, "add", ".gitignore")
	runGit(t, f.git, f.info.Path, "-c", "user.email=ao@example.com", "-c", "user.name=Ao", "commit", "-m", "ignore build")
	f.write("build/out.bin", "x\n")
	if err := f.ws.Destroy(context.Background(), f.info); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	f.ws.waitForDiscards()
	if _, err := os.Stat(f.backupRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a worktree without secrets created an archive: %v", err)
	}
}
