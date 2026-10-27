package review

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/identity"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type countingReviewProofs struct {
	issued []string
	err    error
}

func (c *countingReviewProofs) IssueReview(_ context.Context, reviewID string) (string, error) {
	if c.err != nil {
		return "", c.err
	}
	c.issued = append(c.issued, reviewID)
	return "aop1.proof-for-" + reviewID, nil
}

// ED8-1 F1: a terminal reviewer is launched as a new pane every time, and each launch gives the
// review its own proof. A project or an adapter cannot supply one.
func TestTerminalReviewerLaunchGivesTheReviewItsProof(t *testing.T) {
	for name, run := range map[string]func(l Launcher, spec LaunchSpec) error{
		"spawn": func(l Launcher, spec LaunchSpec) error { _, err := l.Spawn(context.Background(), spec); return err },
		"restore": func(l Launcher, spec LaunchSpec) error {
			_, err := l.RestoreTerminal(context.Background(), spec)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			rt := &fakeRuntime{}
			proofs := &countingReviewProofs{}
			reviewer := &fakeReviewer{env: map[string]string{identity.EnvProof: "aop1.from-the-adapter"}}
			l := NewLauncher(fakeReviewerResolver{reviewer: reviewer, ok: true}, rt, t.TempDir(), WithReviewerProofs(proofs))
			spec := launchSpec()
			spec.ProjectEnv = map[string]string{identity.EnvProof: "aop1.from-the-project"}

			if err := run(l, spec); err != nil {
				t.Fatal(err)
			}
			if got := rt.createCfg.Env[identity.EnvProof]; got != "aop1.proof-for-review-1" {
				t.Fatalf("reviewer pane proof = %q, want the one issued for review-1", got)
			}
			if len(proofs.issued) != 1 || proofs.issued[0] != "review-1" {
				t.Fatalf("issued for %v, want exactly review-1 once", proofs.issued)
			}
		})
	}
}

func TestReviewerLaunchFailsWhenItsProofCannotBeIssued(t *testing.T) {
	rt := &fakeRuntime{}
	l := NewLauncher(fakeReviewerResolver{reviewer: &fakeReviewer{}, ok: true}, rt, t.TempDir(),
		WithReviewerProofs(&countingReviewProofs{err: errors.New("identity store is down")}))
	if _, err := l.Spawn(context.Background(), launchSpec()); err == nil {
		t.Fatal("a reviewer started with no way to identify itself")
	}
	if rt.createCfg.SessionID != "" {
		t.Fatal("the reviewer pane was created although its proof could not be issued")
	}
}

func TestNoProofIssuerMeansNoReviewerProof(t *testing.T) {
	rt := &fakeRuntime{}
	l := NewLauncher(fakeReviewerResolver{reviewer: &fakeReviewer{env: map[string]string{identity.EnvProof: "aop1.from-the-adapter"}}, ok: true}, rt, t.TempDir())
	if _, err := l.Spawn(context.Background(), launchSpec()); err != nil {
		t.Fatal(err)
	}
	if _, ok := rt.createCfg.Env[identity.EnvProof]; ok {
		t.Fatal("a proof the daemon did not issue reached the reviewer")
	}
}

type recordingChatStart struct {
	ReviewerChatController
	started ReviewerChatStart
}

func (c *recordingChatStart) StartReviewChat(_ context.Context, cfg ReviewerChatStart) (string, error) {
	c.started = cfg
	return "provider-1", nil
}

func (c *recordingChatStart) RestoreReviewChat(_ context.Context, cfg ReviewerChatStart) (string, error) {
	c.started = cfg
	return "provider-1", nil
}

type chatProfile struct{}

func (chatProfile) ReviewChatHarness() domain.AgentHarness { return domain.HarnessCodex }

// A chat reviewer's provider may outlive the daemon: starting or restoring the chat must not
// issue anything by itself (the live host keeps its environment and proof). The proof is minted
// by PrepareEnv, which the chat service calls only when it really launches the provider.
func TestChatReviewerProofIsIssuedOnlyWhenTheProviderIsLaunched(t *testing.T) {
	proofs := &countingReviewProofs{}
	chat := &recordingChatStart{}
	l := NewLauncher(fakeReviewerResolver{reviewer: &fakeReviewer{}, ok: true}, &fakeRuntime{}, t.TempDir(),
		WithReviewerChat(chat), WithReviewerProofs(proofs)).(*agentLauncher)
	promptFile := filepath.Join(t.TempDir(), "system.md")
	if err := os.WriteFile(promptFile, []byte("review"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, restore := range []bool{false, true} {
		if _, err := l.startReviewerChat(context.Background(), launchSpec(), ports.ReviewInvocation{SystemPromptFile: promptFile, Prompt: "go"}, chatProfile{}, restore); err != nil {
			t.Fatal(err)
		}
	}
	if len(proofs.issued) != 0 {
		t.Fatalf("starting/restoring a reviewer chat issued %v; only a real provider launch may", proofs.issued)
	}
	if _, ok := chat.started.Env[identity.EnvProof]; ok {
		t.Fatal("the base environment carries a proof")
	}
	if chat.started.PrepareEnv == nil {
		t.Fatal("no PrepareEnv: the chat service has no way to mint a proof at launch")
	}

	env, err := chat.started.PrepareEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if env[identity.EnvProof] != "aop1.proof-for-review-1" || env["AO_REVIEW_SESSION_ID"] != "review-1" {
		t.Fatalf("prepared env = proof %q, review %q", env[identity.EnvProof], env["AO_REVIEW_SESSION_ID"])
	}
	if len(proofs.issued) != 1 {
		t.Fatalf("issued %v, want one proof for the one launch", proofs.issued)
	}
}

// P5: a harness that filters its shell commands' environment (Codex) gets the reviewer's proof
// through a private file. The invocation carries only the file's PATH; after the launch environment
// is built the proof is written there (0600, replaced on every launch) and the CLI reads it back.
func TestTerminalReviewerProofIsPlacedInAPrivateFileTheInvocationNames(t *testing.T) {
	rt := &fakeRuntime{}
	proofs := &countingReviewProofs{}
	reviewer := &fakeReviewer{}
	dataDir := t.TempDir()
	l := NewLauncher(fakeReviewerResolver{reviewer: reviewer, ok: true}, rt, dataDir, WithReviewerProofs(proofs))
	spec := launchSpec()
	if _, err := l.Spawn(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dataDir, "prompts", string(spec.WorkerID), "reviewer", reviewerProofFileName)
	if reviewer.gotInv.ProofFilePath != want {
		t.Fatalf("invocation proof path = %q, want %q", reviewer.gotInv.ProofFilePath, want)
	}
	info, err := os.Stat(want)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("proof file mode = %v, want 0600", info.Mode().Perm())
	}
	read := func() string {
		b, err := os.ReadFile(want)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(b))
	}
	if got := read(); got != "aop1.proof-for-review-1" {
		t.Fatalf("the file holds %q, want the issued proof", got)
	}
	// a relaunch replaces it
	proofs.issued = nil
	if _, err := l.RestoreTerminal(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if got := read(); got != "aop1.proof-for-review-1" {
		t.Fatalf("after a relaunch the file holds %q", got)
	}
	entries, _ := os.ReadDir(filepath.Dir(want))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".reviewer-proof-") {
			t.Errorf("a temp proof file was left behind: %s", e.Name())
		}
	}
}

func TestNoProofFileIsNamedWhenNoProofsAreIssued(t *testing.T) {
	reviewer := &fakeReviewer{}
	l := NewLauncher(fakeReviewerResolver{reviewer: reviewer, ok: true}, &fakeRuntime{}, t.TempDir())
	if _, err := l.Spawn(context.Background(), launchSpec()); err != nil {
		t.Fatal(err)
	}
	if reviewer.gotInv.ProofFilePath != "" {
		t.Fatalf("proof path %q named although no proof is issued", reviewer.gotInv.ProofFilePath)
	}
}

func TestAReviewerWhoseProofFileCannotBeWrittenDoesNotStart(t *testing.T) {
	rt := &fakeRuntime{}
	dataDir := t.TempDir()
	l := NewLauncher(fakeReviewerResolver{reviewer: &fakeReviewer{}, ok: true}, rt, dataDir, WithReviewerProofs(&countingReviewProofs{}))
	spec := launchSpec()
	// a directory where the proof file must go: the rename fails
	blocker := filepath.Join(dataDir, "prompts", string(spec.WorkerID), "reviewer", reviewerProofFileName)
	if err := os.MkdirAll(filepath.Join(blocker, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Spawn(context.Background(), spec); err == nil {
		t.Fatal("a reviewer launched although its proof file could not be written")
	}
	if rt.createCfg.SessionID != "" {
		t.Fatal("the reviewer pane was created")
	}
}

// What the launcher writes is exactly what the CLI accepts: a well-formed proof in an owner-only file
// that AO_SESSION_PROOF_FILE names.
func TestAWrittenReviewerProofFileIsWhatTheCLIReads(t *testing.T) {
	proof, err := identity.NewProof()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "prompts", "w1", "reviewer", reviewerProofFileName)
	if err := writeReviewerProofFile(path, proof); err != nil {
		t.Fatal(err)
	}
	getenv := func(k string) string {
		if k == identity.EnvProofFile {
			return path
		}
		return ""
	}
	if got := identity.ProofFromEnv(getenv); got != proof {
		t.Fatalf("the CLI reads %q, want the written proof", got)
	}
	if err := writeReviewerProofFile("", proof); err != nil {
		t.Fatalf("no path must be a no-op: %v", err)
	}
}
