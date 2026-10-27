package gitworktree

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	aoprocess "github.com/aoagents/agent-orchestrator/backend/internal/process"
)

// ForceDestroy removes a worktree whether or not it is dirty, so it owes the
// caller a guarantee that nothing of value goes with it. preserveForRemoval is
// that guarantee, and it runs on the directory that is about to be unlinked:
//
//   - Uncommitted work (modified or staged tracked files, untracked files that
//     .gitignore does not cover) goes into a backup commit under
//     refs/ao-backup/<session>/<UTC timestamp>, which is then VERIFIED (the ref
//     resolves, its parent is HEAD, and every changed path is in its tree).
//   - A detached HEAD that no ref reaches is pinned by a backup ref, so removing
//     the worktree cannot orphan its commits.
//   - Git-ignored secret files (.env*, *.local) are copied into a private archive
//     under the backup root (0700 directories, 0600 files) and are never written
//     into a git object. node_modules and build output are not archived.
//   - A directory that is not a registered worktree is reported so the caller
//     moves it aside instead of deleting it.
//
// Any failure to preserve or to verify returns an error and the caller refuses
// the removal. Unpushed commits and stash entries live in the shared repository
// (removing a worktree never deletes a branch or the stash), so they are
// counted and reported rather than copied.
const (
	backupRefPrefix       = "refs/ao-backup/"
	secretArchiveMaxBytes = 64 << 20
	secretLogNameLimit    = 20
)

// preserveBudget bounds the preservation walk (status, ignored-file listing,
// hashing) that runs under the repo teardown lock; backupMaxBytes bounds how much
// untracked and modified content is copied into the repository's object database.
// Beyond either, ForceDestroy does not copy: it keeps the worktree directory aside
// (see keepInsteadOfRemoving) so nothing is lost and nothing blocks. They are
// variables so tests can shrink them.
var (
	preserveBudget = 60 * time.Second
	backupMaxBytes = int64(512 << 20)
)

// secretPathspecs select git-ignored files that are local secrets and cannot be
// regenerated: .env, .env.*, .envrc and *.local. node_modules is excluded.
var secretPathspecs = []string{
	":(glob)**/.env",
	":(glob)**/.env.*",
	":(glob)**/.envrc",
	":(glob)**/*.local",
	":(exclude,glob)**/node_modules/**",
}

type removalPreservation struct {
	// unregistered is set when the directory exists but git has no worktree
	// registered there; nothing is preserved by ref and the caller moves it aside.
	unregistered bool
	// ref and commit identify the verified backup, empty when the worktree had
	// nothing to preserve.
	ref    string
	commit string
	// changedPaths counts the dirty paths captured in the backup commit.
	changedPaths int
	// pinnedHead is set when ref only pins a detached HEAD no other ref reached.
	pinnedHead      bool
	unpushedCommits int
	stashEntries    int
	ignoredPaths    int
	// secretArchive is the private directory holding copied secret files;
	// secretNames are their relative names (never contents).
	secretArchive string
	secretNames   []string
	branch        string
	head          string
}

type dirtyEntry struct {
	status string
	path   string
}

func backupRootFor(configured, managedRoot string) string {
	if strings.TrimSpace(configured) != "" {
		return filepath.Clean(configured)
	}
	return filepath.Join(filepath.Dir(managedRoot), "backups")
}

// preserveForRemoval secures everything of value in dir (the worktree's current
// location, which may differ from its registered path after a rename-aside)
// before ForceDestroy removes it. path is the registered location.
func (w *Workspace) preserveForRemoval(ctx context.Context, repo, dir, path string, info ports.WorkspaceInfo) (removalPreservation, error) {
	var report removalPreservation
	records, err := w.listRecords(ctx, repo)
	if err != nil {
		return report, fmt.Errorf("cannot list worktrees to verify %q: %w", path, preserveRefusal(err))
	}
	rec, registered := findWorktree(records, path)
	if !registered {
		empty, emptyErr := dirIsEmpty(dir)
		if emptyErr != nil {
			return report, fmt.Errorf("cannot inspect unregistered directory %q: %w", path, preserveRefusal(emptyErr))
		}
		if !empty {
			report.unregistered = true
		}
		return report, nil
	}
	report.branch, report.head = rec.Branch, rec.Head

	entries, err := w.dirtyEntries(ctx, dir)
	if err != nil {
		if isNotGitRepositoryError(err) {
			// The directory is not a usable worktree (its .git is gone): treat it
			// as an unregistered directory, which is moved aside, never deleted.
			report.unregistered = true
			return report, nil
		}
		return report, fmt.Errorf("cannot read the git status of %q: %w", path, preserveRefusal(err))
	}
	if err := checkPreservable(dir, entries); err != nil {
		return report, fmt.Errorf("cannot preserve %q: %w", path, preserveRefusal(err))
	}
	head := w.resolveHead(ctx, dir)
	unreachable := false
	if head != "" && (rec.Detached || rec.Branch == "") {
		reached, reachErr := w.headReachedByARef(ctx, repo, head)
		if reachErr != nil {
			return report, fmt.Errorf("cannot tell whether HEAD %s of %q is reachable: %w", short(head), path, preserveRefusal(reachErr))
		}
		unreachable = !reached
	}
	report.unpushedCommits = w.countUnpushed(ctx, repo, rec.Branch)
	report.stashEntries = w.countStash(ctx, repo, rec.Branch)
	if ignored, err := w.countIgnoredPaths(ctx, dir); err == nil {
		report.ignoredPaths = ignored
	}

	archive, names, err := w.archiveIgnoredSecrets(ctx, dir, info.SessionID)
	if err != nil {
		return report, fmt.Errorf("could not archive the git-ignored secret files of %q: %w", path, preserveRefusal(err))
	}
	report.secretArchive, report.secretNames = archive, names

	if len(entries) == 0 && !unreachable {
		return report, nil
	}
	session := backupSessionName(info.SessionID, path)
	if len(entries) == 0 {
		// Only a detached HEAD needs pinning: nothing is dirty.
		ref, err := w.newBackupRef(ctx, repo, session, "-head", head)
		if err != nil {
			return report, fmt.Errorf("could not pin the detached HEAD %s of %q: %w", short(head), path, preserveRefusal(err))
		}
		if err := w.verifyPinnedHead(ctx, repo, ref, head); err != nil {
			return report, fmt.Errorf("could not verify the pin of detached HEAD %s of %q: %w", short(head), path, preserveRefusal(err))
		}
		report.ref, report.commit, report.pinnedHead = ref, head, true
		return report, nil
	}
	treeSHA, commitSHA, err := w.captureBackupCommit(ctx, dir, head, session)
	if err != nil {
		return report, fmt.Errorf("could not preserve %d uncommitted path(s) of %q (%s): %w", len(entries), path, describeDirty(entries), preserveRefusal(err))
	}
	ref, err := w.backupRefFor(ctx, repo, session, treeSHA, head, commitSHA)
	if err != nil {
		return report, fmt.Errorf("could not record the backup of %q: %w", path, preserveRefusal(err))
	}
	if err := w.verifyBackup(ctx, repo, ref, head, entries); err != nil {
		return report, fmt.Errorf("could not verify the backup of %d uncommitted path(s) of %q (%s): %w", len(entries), path, describeDirty(entries), preserveRefusal(err))
	}
	resolved, err := w.run(ctx, w.binary, "-C", repo, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return report, fmt.Errorf("could not resolve the backup ref %s: %w", ref, preserveRefusal(err))
	}
	report.ref, report.commit = ref, strings.TrimSpace(string(resolved))
	report.changedPaths = len(entries)
	return report, nil
}

// preserveRefusal marks a preservation failure as a refusal so callers that
// classify ports.ErrWorkspaceDirty treat the worktree as one to leave alone.
func preserveRefusal(err error) error {
	return fmt.Errorf("%w (%w)", ports.ErrWorkspaceDirty, err)
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func describeDirty(entries []dirtyEntry) string {
	counts := map[string]int{}
	for _, e := range entries {
		switch {
		case e.status == "??":
			counts["untracked"]++
		case strings.Contains(e.status, "D"):
			counts["deleted"]++
		case e.status[0] != ' ':
			counts["staged"]++
		default:
			counts["modified"]++
		}
	}
	var parts []string
	for _, key := range []string{"modified", "staged", "deleted", "untracked"} {
		if counts[key] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[key], key))
		}
	}
	return strings.Join(parts, ", ")
}

func dirIsEmpty(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return true, nil
		}
		return false, err
	}
	return len(entries) == 0, nil
}

// backupSessionName makes the session id safe to use as a ref and directory
// component.
func backupSessionName(sessionID domain.SessionID, path string) string {
	name := strings.TrimSpace(string(sessionID))
	if name == "" {
		name = filepath.Base(path)
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), ".")
	if out == "" {
		return "session"
	}
	return out
}

// dirtyEntries lists every modified, staged, deleted or untracked (non-ignored)
// path in dir, with untracked directories expanded to their files.
func (w *Workspace) dirtyEntries(ctx context.Context, dir string) ([]dirtyEntry, error) {
	out, err := w.run(ctx, w.binary, "-C", dir, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return nil, fmt.Errorf("gitworktree: status %q: %w", dir, err)
	}
	var entries []dirtyEntry
	records := strings.Split(string(out), "\x00")
	for i := 0; i < len(records); i++ {
		rec := records[i]
		if len(rec) < 4 {
			continue
		}
		status, path := rec[:2], rec[3:]
		if status[0] == 'R' || status[0] == 'C' {
			i++ // the next record is the rename/copy source
		}
		entries = append(entries, dirtyEntry{status: status, path: path})
	}
	return entries, nil
}

func (w *Workspace) resolveHead(ctx context.Context, dir string) string {
	out, err := w.run(ctx, w.binary, revParseHeadArgs(dir)...)
	if err != nil {
		return "" // unborn HEAD
	}
	return strings.TrimSpace(string(out))
}

func (w *Workspace) headReachedByARef(ctx context.Context, repo, head string) (bool, error) {
	out, err := w.run(ctx, w.binary, "-C", repo, "for-each-ref", "--count=1", "--format=%(refname)", "--contains", head, "refs/heads", "refs/remotes", "refs/tags", "refs/ao", backupRefPrefix)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) != "", nil
}

// countUnpushed counts commits on the branch that no remote-tracking ref has.
// They stay on the branch after the worktree is removed; this only reports them.
func (w *Workspace) countUnpushed(ctx context.Context, repo, branch string) int {
	if branch == "" {
		return 0
	}
	out, err := w.run(ctx, w.binary, "-C", repo, "rev-list", "--count", "refs/heads/"+branch, "--not", "--remotes")
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return n
}

// countStash counts stash entries made on the branch. The stash is shared by
// the repository and survives the worktree's removal.
func (w *Workspace) countStash(ctx context.Context, repo, branch string) int {
	if branch == "" {
		return 0
	}
	out, err := w.run(ctx, w.binary, "-C", repo, "stash", "list", "--format=%gs")
	if err != nil {
		return 0
	}
	n := 0
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "WIP on "+branch+":") || strings.HasPrefix(line, "On "+branch+":") {
			n++
		}
	}
	return n
}

// captureBackupCommit builds a commit holding every tracked change and every
// non-ignored untracked file, through a temporary index so the worktree's own
// index is never touched. Ignored files are not included (secrets go to the
// private archive instead).
func (w *Workspace) captureBackupCommit(ctx context.Context, dir, head, session string) (treeSHA, commitSHA string, err error) {
	tmp, err := os.CreateTemp("", "ao-backup-idx-*")
	if err != nil {
		return "", "", fmt.Errorf("reserve temp index path: %w", err)
	}
	idx := tmp.Name()
	_ = tmp.Close()
	_ = os.Remove(idx)
	defer func() { _ = os.Remove(idx) }()
	add := aoprocess.CommandContext(ctx, w.binary, addAllTempIndexArgs(dir)...)
	add.Env = append(os.Environ(), "GIT_INDEX_FILE="+idx)
	if out, err := add.CombinedOutput(); err != nil {
		return "", "", commandError{args: append([]string{w.binary}, addAllTempIndexArgs(dir)...), output: string(out), err: err}
	}
	write := aoprocess.CommandContext(ctx, w.binary, writeTreeArgs(dir)...)
	write.Env = append(os.Environ(), "GIT_INDEX_FILE="+idx)
	out, err := write.CombinedOutput()
	if err != nil {
		return "", "", commandError{args: append([]string{w.binary}, writeTreeArgs(dir)...), output: string(out), err: err}
	}
	treeSHA = strings.TrimSpace(string(out))
	commitOut, err := w.run(ctx, w.binary, commitTreeArgs(dir, treeSHA, head, "ao backup before removal "+session)...)
	if err != nil {
		return "", "", fmt.Errorf("commit-tree: %w", err)
	}
	return treeSHA, strings.TrimSpace(string(commitOut)), nil
}

// backupRefFor returns the ref that holds the backup: an existing one for this
// session whose tree and parent are identical (a retry does not spawn a new
// ref), otherwise a new refs/ao-backup/<session>/<timestamp>.
func (w *Workspace) backupRefFor(ctx context.Context, repo, session, treeSHA, head, commitSHA string) (string, error) {
	existing, err := w.run(ctx, w.binary, "-C", repo, "for-each-ref", "--format=%(refname)", backupRefPrefix+session+"/")
	if err != nil {
		return "", err
	}
	for _, ref := range strings.Fields(string(existing)) {
		tree, treeErr := w.run(ctx, w.binary, "-C", repo, "rev-parse", "--verify", ref+"^{tree}")
		if treeErr != nil || strings.TrimSpace(string(tree)) != treeSHA {
			continue
		}
		parent, parentErr := w.run(ctx, w.binary, "-C", repo, "rev-parse", "--verify", "-q", ref+"^")
		parentSHA := strings.TrimSpace(string(parent))
		if parentErr != nil {
			parentSHA = ""
		}
		if parentSHA == head {
			return ref, nil
		}
	}
	return w.newBackupRef(ctx, repo, session, "", commitSHA)
}

func (w *Workspace) newBackupRef(ctx context.Context, repo, session, suffix, commitSHA string) (string, error) {
	stamp := time.Now().UTC().Format("20060102T150405Z")
	for i := 0; i < 100; i++ {
		name := stamp + suffix
		if i > 0 {
			name = fmt.Sprintf("%s%s-%d", stamp, suffix, i+1)
		}
		ref := backupRefPrefix + session + "/" + name
		if _, err := w.run(ctx, w.binary, "-C", repo, "rev-parse", "--verify", "-q", ref); err == nil {
			continue // taken
		}
		// The zero old-value makes update-ref create the ref only if absent.
		if _, err := w.run(ctx, w.binary, "-C", repo, "update-ref", ref, commitSHA, strings.Repeat("0", 40)); err != nil {
			return "", err
		}
		return ref, nil
	}
	return "", errors.New("no available backup ref name")
}

// verifyBackup proves the backup is real before anything is removed: the ref
// resolves to a commit whose parent is HEAD, and every path that was dirty is in
// its tree (a deleted path is allowed to be absent).
func (w *Workspace) verifyBackup(ctx context.Context, repo, ref, head string, entries []dirtyEntry) error {
	if _, err := w.run(ctx, w.binary, "-C", repo, "cat-file", "-e", ref+"^{commit}"); err != nil {
		return fmt.Errorf("backup commit missing: %w", err)
	}
	if head != "" {
		parent, err := w.run(ctx, w.binary, "-C", repo, "rev-parse", "--verify", ref+"^")
		if err != nil || strings.TrimSpace(string(parent)) != head {
			return errors.New("backup commit is not parented on HEAD")
		}
	}
	listing, err := w.run(ctx, w.binary, "-C", repo, "ls-tree", "-r", "-z", "--name-only", ref)
	if err != nil {
		return fmt.Errorf("list backup tree: %w", err)
	}
	inTree := map[string]bool{}
	for _, name := range strings.Split(string(listing), "\x00") {
		if name != "" {
			inTree[name] = true
		}
	}
	for _, e := range entries {
		if strings.Contains(e.status, "D") {
			continue
		}
		if !inTree[e.path] {
			return fmt.Errorf("path %q is not in the backup", e.path)
		}
	}
	return nil
}

func (w *Workspace) verifyPinnedHead(ctx context.Context, repo, ref, head string) error {
	out, err := w.run(ctx, w.binary, "-C", repo, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil || strings.TrimSpace(string(out)) != head {
		return errors.New("pin does not resolve to the detached HEAD")
	}
	return nil
}

// archiveIgnoredSecrets copies git-ignored secret files (.env*, *.local) from
// dir into a private archive and verifies each copy. It returns the archive
// directory and the relative file names (never contents). With no such files it
// returns nothing and creates nothing.
func (w *Workspace) archiveIgnoredSecrets(ctx context.Context, dir string, sessionID domain.SessionID) (string, []string, error) {
	names, err := w.listIgnoredSecrets(ctx, dir)
	if err != nil {
		return "", nil, err
	}
	if len(names) == 0 {
		return "", nil, nil
	}
	session := backupSessionName(sessionID, dir)
	sessionDir := filepath.Join(w.backupRoot, session)
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		return "", nil, fmt.Errorf("create backup directory: %w", err)
	}
	if err := os.Chmod(sessionDir, 0o700); err != nil { //nolint:gosec // a directory needs its search bit; 0700 keeps it private to the owner
		return "", nil, fmt.Errorf("secure backup directory: %w", err)
	}
	stamp := time.Now().UTC().Format("20060102T150405Z")
	archive := ""
	for i := 0; i < 100; i++ {
		candidate := filepath.Join(sessionDir, stamp)
		if i > 0 {
			candidate = fmt.Sprintf("%s-%d", candidate, i+1)
		}
		if err := os.Mkdir(candidate, 0o700); err == nil {
			archive = candidate
			break
		} else if !errors.Is(err, os.ErrExist) {
			return "", nil, fmt.Errorf("create archive directory: %w", err)
		}
	}
	if archive == "" {
		return "", nil, errors.New("no available archive directory name")
	}
	if err := os.Chmod(archive, 0o700); err != nil { //nolint:gosec // a directory needs its search bit; 0700 keeps it private to the owner
		_ = os.RemoveAll(archive)
		return "", nil, fmt.Errorf("secure archive directory: %w", err)
	}
	var archived []string
	var manifest bytes.Buffer
	for _, name := range names {
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			_ = os.RemoveAll(archive)
			return "", nil, fmt.Errorf("unsafe secret file name %q", name)
		}
		src := filepath.Join(dir, name)
		info, err := os.Lstat(src)
		if err != nil {
			_ = os.RemoveAll(archive)
			return "", nil, fmt.Errorf("inspect %q: %w", name, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			// Record where it pointed; never follow a link out of the worktree.
			target, _ := os.Readlink(src)
			fmt.Fprintf(&manifest, "%s\tsymlink -> %s\n", name, target)
			archived = append(archived, name)
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if info.Size() > secretArchiveMaxBytes {
			_ = os.RemoveAll(archive)
			return "", nil, fmt.Errorf("%q is too large to archive (%d bytes)", name, info.Size())
		}
		if err := copyPrivateFile(archive, src, filepath.Join(archive, "files", name)); err != nil {
			_ = os.RemoveAll(archive)
			return "", nil, fmt.Errorf("archive %q: %w", name, err)
		}
		fmt.Fprintf(&manifest, "%s\t%d bytes\n", name, info.Size())
		archived = append(archived, name)
	}
	if len(archived) == 0 {
		_ = os.RemoveAll(archive)
		return "", nil, nil
	}
	if err := os.WriteFile(filepath.Join(archive, "MANIFEST"), manifest.Bytes(), 0o600); err != nil {
		_ = os.RemoveAll(archive)
		return "", nil, fmt.Errorf("write manifest: %w", err)
	}
	return archive, archived, nil
}

// archiveSecretsBeforeRemoval is Destroy's share of the preservation contract.
// Destroy refuses a dirty worktree, but a CLEAN worktree still carries
// git-ignored files that are removed with it; build output and caches are
// regenerable, local secrets (.env*, *.local) are not. They are archived
// privately before the removal, and the removal is refused (an error wrapping
// ports.ErrWorkspaceDirty that names what failed and where the archive would
// go) when the archive cannot be written. dir is where the files are now (the
// renamed-aside directory on the fast path); path is the registered location.
func (w *Workspace) archiveSecretsBeforeRemoval(ctx context.Context, repo, dir, path string, info ports.WorkspaceInfo) (string, error) {
	if _, err := os.Lstat(dir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil // nothing on disk to protect
		}
		return "", fmt.Errorf("gitworktree: refusing to remove %q: cannot inspect it: %w", path, preserveRefusal(err))
	}
	records, err := w.listRecords(ctx, repo)
	if err != nil {
		return "", fmt.Errorf("gitworktree: refusing to remove %q: cannot list worktrees to check it for secret files: %w", path, preserveRefusal(err))
	}
	if _, registered := findWorktree(records, path); !registered {
		return "", nil // not a worktree git can list files for
	}
	return w.archiveRegisteredSecrets(ctx, dir, path, info)
}

// archiveRegisteredSecrets archives the secret files of a worktree already known
// to be registered (Destroy's fast path has just established that).
func (w *Workspace) archiveRegisteredSecrets(ctx context.Context, dir, path string, info ports.WorkspaceInfo) (string, error) {
	archive, names, err := w.archiveIgnoredSecrets(ctx, dir, info.SessionID)
	if err != nil {
		return "", fmt.Errorf("gitworktree: refusing to remove %q: could not archive its git-ignored secret files (.env*) to %s: %w",
			path, filepath.Join(w.backupRoot, backupSessionName(info.SessionID, path)), preserveRefusal(err))
	}
	if len(names) > 0 {
		w.logPreservation(path, info, removalPreservation{secretArchive: archive, secretNames: names})
	}
	return archive, nil
}

// dropArchive deletes a secret archive that turned out to be redundant because
// the removal it protected did not happen (the directory is still there, or was
// kept aside whole). It never touches anything outside the archive directory.
func dropArchive(archive string) {
	if archive != "" {
		_ = os.RemoveAll(archive)
	}
}

// listIgnoredSecrets lists the git-ignored secret files in dir (sorted, relative).
func (w *Workspace) listIgnoredSecrets(ctx context.Context, dir string) ([]string, error) {
	args := append([]string{"-C", dir, "ls-files", "--others", "--ignored", "--exclude-standard", "-z", "--"}, secretPathspecs...)
	out, err := w.run(ctx, w.binary, args...)
	if err != nil {
		return nil, fmt.Errorf("list ignored secret files: %w", err)
	}
	var names []string
	for _, name := range strings.Split(string(out), "\x00") {
		if name = filepath.Clean(strings.TrimSpace(name)); name != "" && name != "." {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}

// copyPrivateFile copies src to dst with private permissions (every directory
// created under root 0700, the file 0600) and verifies the copy by content hash.
func copyPrivateFile(root, src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	for dir := filepath.Dir(dst); strings.HasPrefix(dir, root); dir = filepath.Dir(dir) {
		if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // a directory needs its search bit; 0700 keeps it private to the owner
			return err
		}
		if dir == root {
			break
		}
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }() // read-only source
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	hasher := sha256.New()
	if _, err := io.Copy(io.MultiWriter(out, hasher), in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := os.Chmod(dst, 0o600); err != nil {
		return err
	}
	check, err := os.Open(dst)
	if err != nil {
		return err
	}
	defer func() { _ = check.Close() }() // read-only verification handle
	verify := sha256.New()
	if _, err := io.Copy(verify, check); err != nil {
		return err
	}
	if !bytes.Equal(hasher.Sum(nil), verify.Sum(nil)) {
		return errors.New("copy does not match the source")
	}
	return nil
}

// checkPreservable rejects what cannot be copied into a backup commit: a nested
// git repository (status lists it as "dir/" while a backup tree can only hold a
// gitlink with no contents) and more content than backupMaxBytes.
func checkPreservable(dir string, entries []dirtyEntry) error {
	var total int64
	for _, e := range entries {
		if strings.HasSuffix(e.path, "/") {
			return fmt.Errorf("it contains a nested git repository (%s) that a backup commit cannot hold", e.path)
		}
		if strings.Contains(e.status, "D") {
			continue
		}
		if info, err := os.Lstat(filepath.Join(dir, e.path)); err == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
	}
	if total > backupMaxBytes {
		return fmt.Errorf("its uncommitted files total %d bytes, over the %d byte limit for a backup commit", total, backupMaxBytes)
	}
	return nil
}

// PreflightRemoval says, without changing anything, whether ForceDestroy could
// keep this worktree's work safe by backing it up. A caller that is about to
// terminate a session before force-removing its worktree (RetireForReplacement)
// asks first, so a worktree that cannot be backed up leaves the session running
// instead of stranding it. A nil result is not a promise: ForceDestroy still
// preserves again at the moment of removal and, if that then fails, keeps the
// directory aside rather than deleting it.
func (w *Workspace) PreflightRemoval(ctx context.Context, info ports.WorkspaceInfo) error {
	if info.Path == "" {
		return fmt.Errorf("%w: empty path", ErrUnsafePath)
	}
	repo, err := w.repoPathForInfo(info)
	if err != nil {
		return err
	}
	path, err := w.validateManagedPath(info.Path)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // nothing on disk to lose
		}
		return fmt.Errorf("cannot inspect %q: %w", path, preserveRefusal(err))
	}
	ctx, cancel := context.WithTimeout(ctx, preserveBudget)
	defer cancel()
	records, err := w.listRecords(ctx, repo)
	if err != nil {
		return fmt.Errorf("cannot list worktrees to verify %q: %w", path, preserveRefusal(err))
	}
	if _, registered := findWorktree(records, path); !registered {
		return nil // an unregistered directory is kept aside, never deleted
	}
	entries, err := w.dirtyEntries(ctx, path)
	if err != nil {
		if isNotGitRepositoryError(err) {
			return nil
		}
		return fmt.Errorf("cannot read the git status of %q: %w", path, preserveRefusal(err))
	}
	if err := checkPreservable(path, entries); err != nil {
		return fmt.Errorf("worktree %q has work that cannot be backed up: %w", path, preserveRefusal(err))
	}
	// Secrets: only prove the private archive location is writable.
	names, err := w.listIgnoredSecrets(ctx, path)
	if err != nil {
		return fmt.Errorf("could not list the git-ignored secret files of %q: %w", path, preserveRefusal(err))
	}
	if len(names) > 0 {
		if err := w.probeBackupRoot(info.SessionID, path); err != nil {
			return fmt.Errorf("cannot archive the git-ignored secret files (.env*) of %q to %s: %w",
				path, filepath.Join(w.backupRoot, backupSessionName(info.SessionID, path)), preserveRefusal(err))
		}
	}
	return nil
}

// probeBackupRoot checks that a private archive directory can be created.
func (w *Workspace) probeBackupRoot(sessionID domain.SessionID, path string) error {
	dir := filepath.Join(w.backupRoot, backupSessionName(sessionID, path))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	probe, err := os.CreateTemp(dir, ".probe-*")
	if err != nil {
		return err
	}
	name := probe.Name()
	_ = probe.Close()
	return os.Remove(name)
}

// keepInsteadOfRemoving is what ForceDestroy does when it cannot prove a
// worktree's work is saved: the directory is kept whole beside its original path
// (<path>.kept, or .stray for a directory git does not know), the registration is
// pruned so the branch is released, and the caller is told by a warning, not an
// error, so a replacement or shutdown is never blocked and nothing is deleted.
// Only if even that cannot be done is the removal refused. The redundant secret
// archive is dropped because the secrets are still in the kept directory.
func (w *Workspace) keepInsteadOfRemoving(ctx context.Context, repo, from, path, suffix, archive string, reason error) error {
	kept, err := keepBeside(from, path, suffix)
	if err != nil {
		if from != path {
			w.undiscard(from, path) // restores it, or keeps it somewhere the sweep never deletes
		}
		return fmt.Errorf("gitworktree: refusing to remove %q: %w (and it could not be kept aside: %w)", path, reason, err)
	}
	dropArchive(archive)
	_, pruneErr := w.run(ctx, w.binary, worktreePruneArgs(repo)...)
	stillRegistered := false
	if pruneErr == nil {
		if records, listErr := w.listRecords(ctx, repo); listErr == nil {
			_, stillRegistered = findWorktree(records, path)
		}
	}
	if pruneErr != nil || stillRegistered {
		// The registration would keep holding the branch while its directory is
		// elsewhere: put the directory back and refuse.
		if renameErr := os.Rename(kept, path); renameErr != nil {
			w.logf("gitworktree: kept %q at %q but could not release its registration or restore it: %v", path, kept, renameErr)
			return fmt.Errorf("gitworktree: refusing to remove %q: its work is kept at %q (%w; the registration could not be released)", path, kept, reason)
		}
		if pruneErr != nil {
			return fmt.Errorf("gitworktree: worktree prune: %w", pruneErr)
		}
		return fmt.Errorf("gitworktree: refusing to remove %q: %w (the registration could not be released)", path, reason)
	}
	if w.logger != nil {
		w.logger.Warn("gitworktree: could not back the worktree up, so its directory was kept aside instead of being deleted",
			"path", path, "keptAt", kept, "reason", reason.Error())
	}
	return nil
}

// logPreservation records exactly where preserved work went: the backup ref and
// commit, the private secret archive and the NAMES (never contents) of archived
// files, plus the commits and stash entries that stay on the branch.
func (w *Workspace) logPreservation(path string, info ports.WorkspaceInfo, r removalPreservation) {
	if w.logger == nil {
		return
	}
	if r.ref == "" && r.secretArchive == "" && r.unpushedCommits == 0 && r.stashEntries == 0 && r.ignoredPaths == 0 {
		return
	}
	names := r.secretNames
	if len(names) > secretLogNameLimit {
		names = names[:secretLogNameLimit]
	}
	w.logger.Warn("gitworktree: preserved work before force removal",
		"session", string(info.SessionID),
		"path", path,
		"branch", r.branch,
		"head", r.head,
		"backupRef", r.ref,
		"backupCommit", r.commit,
		"changedPaths", r.changedPaths,
		"pinnedDetachedHead", r.pinnedHead,
		"unpushedCommitsOnBranch", r.unpushedCommits,
		"stashEntriesOnBranch", r.stashEntries,
		"ignoredPathsNotPreserved", r.ignoredPaths,
		"secretArchive", r.secretArchive,
		"secretFileCount", len(r.secretNames),
		"secretFileNames", names,
	)
}
