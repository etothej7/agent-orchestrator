package tmux

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/identity"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

const testProof = "aop1.THIS-MUST-NEVER-BE-IN-A-COMMAND-LINE"

func proofConfig(t *testing.T) ports.RuntimeConfig {
	t.Helper()
	return ports.RuntimeConfig{
		// the launch command otherwise ends in an interactive shell, which a test cannot wait on
		ExitOnCommandCompletion: true,
		WorkspacePath:           t.TempDir(),
		Argv:                    []string{"printenv", identity.EnvProof},
		Env:                     map[string]string{identity.EnvProof: testProof, "AO_SESSION_ID": "mer-1"},
	}
}

func TestLaunchCommandNeverCarriesTheIdentityProofInline(t *testing.T) {
	cfg := proofConfig(t)
	if cmd := buildLaunchCommand(cfg); strings.Contains(cmd, testProof) || strings.Contains(cmd, identity.EnvProof+"=") {
		t.Fatalf("the proof is in the launch command line (visible to ps): %q", cmd)
	}
	cmd := buildLaunchCommandWithHandoff(cfg, "/tmp/ao-launch-x/env")
	if strings.Contains(cmd, testProof) {
		t.Fatalf("the proof is in the launch command line even with a handoff file: %q", cmd)
	}
	for _, want := range []string{". '/tmp/ao-launch-x/env' || exit; rm -rf '/tmp/ao-launch-x';", "export AO_SESSION_ID='mer-1';"} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("launch command missing %q in %q", want, cmd)
		}
	}
}

func TestLaunchHandoffIsPrivateAndOnlyExistsWhenThereIsAProof(t *testing.T) {
	none, cleanup, err := writeLaunchHandoff(ports.RuntimeConfig{Env: map[string]string{"AO_SESSION_ID": "mer-1"}})
	if err != nil || none != "" {
		t.Fatalf("handoff without a proof = %q, %v; want none", none, err)
	}
	cleanup()

	path, cleanup, err := writeLaunchHandoff(proofConfig(t))
	if err != nil || path == "" {
		t.Fatalf("handoff = %q, %v", path, err)
	}
	defer cleanup()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("handoff file mode = %v, want 0600", info.Mode().Perm())
	}
	dir, err := os.Stat(filepath.Dir(path))
	if err != nil || dir.Mode().Perm() != 0o700 {
		t.Fatalf("handoff directory = %v (%v), want mode 0700", dir, err)
	}
	body, _ := os.ReadFile(path)
	if string(body) != "export "+identity.EnvProof+"='"+testProof+"'\n" {
		t.Fatalf("handoff content = %q", body)
	}
	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("cleanup left the handoff file behind")
	}
}

// The real mechanism, with a real shell: the launched command sees the proof in its environment,
// the command line the shell was given never contained it, and the handoff directory is gone as
// soon as the command runs.
func TestLaunchHandoffDeliversTheProofToTheLaunchedCommandAndDeletesItself(t *testing.T) {
	cfg := proofConfig(t)
	path, cleanup, err := writeLaunchHandoff(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	cmd := buildLaunchCommandWithHandoff(cfg, path)
	if strings.Contains(cmd, testProof) {
		t.Fatalf("proof in command line: %q", cmd)
	}
	out, err := exec.Command("sh", "-c", cmd).CombinedOutput() //nolint:gosec // the command is built by this test
	if err != nil {
		t.Fatalf("launch command: %v\n%s", err, out)
	}
	if strings.TrimSpace(string(out)) != testProof {
		t.Fatalf("the launched command saw %q, want the proof", out)
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatal("the handoff directory still exists after the launch command ran")
	}
}

// A pane that cannot read its handoff must not run the agent without an identity.
func TestLaunchWithAMissingHandoffExitsInsteadOfRunning(t *testing.T) {
	cfg := proofConfig(t)
	cfg.Argv = []string{"echo", "AGENT-RAN"}
	cmd := buildLaunchCommandWithHandoff(cfg, filepath.Join(t.TempDir(), "gone", "env"))
	out, _ := exec.Command("sh", "-c", cmd).CombinedOutput() //nolint:gosec // the command is built by this test
	if strings.Contains(string(out), "AGENT-RAN") {
		t.Fatalf("the agent ran although its identity handoff was missing: %q", out)
	}
}
