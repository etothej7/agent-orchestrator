package tmux

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/identity"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// privateEnvKeys are launch variables that must never appear in a command line. The launch
// command is a shell string handed to tmux, so everything exported inline sits in the pane
// shell's argv, which any process of the same user can read with ps for as long as the pane
// lives. A session's identity proof is such a variable: it reaches the pane through a private
// file the pane's shell reads once and deletes.
var privateEnvKeys = map[string]bool{identity.EnvProof: true}

func isPrivateEnvKey(key string) bool { return privateEnvKeys[key] }

// writeLaunchHandoff writes the private launch variables to a file only this user can read
// (mode 0600 in a 0700 directory) and returns its path; path is "" when there is nothing
// private to hand over. The pane's shell sources the file and removes its directory as the
// first thing it does (see buildLaunchCommandWithHandoff). cleanup removes the directory when
// the launch never started a pane.
func writeLaunchHandoff(cfg ports.RuntimeConfig) (path string, cleanup func(), err error) {
	keys := make([]string, 0, len(privateEnvKeys))
	for key := range privateEnvKeys {
		if cfg.Env[key] != "" {
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		return "", func() {}, nil
	}
	sort.Strings(keys)
	sweepStaleLaunchDirs(os.TempDir(), staleLaunchDirAge)
	dir, err := os.MkdirTemp("", "ao-launch-")
	if err != nil {
		return "", nil, fmt.Errorf("tmux runtime: create private launch directory: %w", err)
	}
	cleanup = func() { _ = os.RemoveAll(dir) }
	var b strings.Builder
	for _, key := range keys {
		b.WriteString("export ")
		b.WriteString(key)
		b.WriteString("=")
		b.WriteString(shellQuote(cfg.Env[key]))
		b.WriteString("\n")
	}
	path = filepath.Join(dir, "env")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("tmux runtime: write private launch variables: %w", err)
	}
	return path, cleanup, nil
}

// staleLaunchDirAge is how long a launch directory may exist before a later launch removes it. A
// pane sources and deletes its directory within moments of starting; an hour means the launch died
// before that and nothing will ever read the file.
const staleLaunchDirAge = time.Hour

// sweepStaleLaunchDirs removes launch directories (ao-launch-*, directories only, not symlinks)
// older than maxAge from dir. Best effort: it never fails a launch.
func sweepStaleLaunchDirs(dir string, maxAge time.Duration) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-maxAge)
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "ao-launch-") || !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		if info, err := entry.Info(); err == nil && info.ModTime().Before(cutoff) {
			_ = os.RemoveAll(filepath.Join(dir, entry.Name()))
		}
	}
}
