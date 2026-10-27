package sessionmanager

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/identity"
)

type memoryProofStore struct{ hashes map[domain.SessionID]string }

func (m *memoryProofStore) SetSessionProofHash(_ context.Context, id domain.SessionID, hash string, _ time.Time) error {
	m.hashes[id] = hash
	return nil
}

func (m *memoryProofStore) SessionIdentityByProofHash(_ context.Context, hash string) (domain.SessionID, string, bool, error) {
	for id, h := range m.hashes {
		if h == hash {
			return id, h, true, nil
		}
	}
	return "", "", false, nil
}

func (m *memoryProofStore) DeleteSessionProof(_ context.Context, id domain.SessionID) error {
	delete(m.hashes, id)
	return nil
}

type liveSessions map[domain.SessionID]domain.SessionRecord

func (l liveSessions) GetSession(_ context.Context, id domain.SessionID) (domain.SessionRecord, bool, error) {
	rec, ok := l[id]
	return rec, ok, nil
}

type failingProofIssuer struct{}

func (failingProofIssuer) Issue(context.Context, domain.SessionID) (string, error) {
	return "", errors.New("identity store is down")
}

func proofManager(proofs ProofIssuer) *Manager {
	return &Manager{
		dataDir:    "/data",
		proofs:     proofs,
		executable: func() (string, error) { return filepath.Join("/opt", "aod", "ao"), nil },
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestLaunchGivesTheSessionItsOwnIdentityProofAndOnlyThatSession(t *testing.T) {
	store := &memoryProofStore{hashes: map[domain.SessionID]string{}}
	resolver := identity.NewResolver(store, liveSessions{
		"mer-1": {ID: "mer-1", ProjectID: "mer", Kind: domain.KindWorker},
		"mer-2": {ID: "mer-2", ProjectID: "mer", Kind: domain.KindOrchestrator},
	})
	manager := proofManager(identity.NewIssuer(store, nil))

	one, _, err := manager.launchRuntimeEnv("mer-1", "mer", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	two, _, err := manager.launchRuntimeEnv("mer-2", "mer", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if one[identity.EnvProof] == "" || one[identity.EnvProof] == two[identity.EnvProof] {
		t.Fatalf("proofs = %q and %q: each session needs its own", one[identity.EnvProof], two[identity.EnvProof])
	}
	caller, err := resolver.Resolve(context.Background(), one[identity.EnvProof])
	if err != nil || caller.Session != "mer-1" || caller.SessionKind != domain.KindWorker {
		t.Fatalf("worker proof resolved to %+v, %v", caller, err)
	}
	caller, err = resolver.Resolve(context.Background(), two[identity.EnvProof])
	if err != nil || caller.Session != "mer-2" || caller.SessionKind != domain.KindOrchestrator {
		t.Fatalf("manager proof resolved to %+v, %v", caller, err)
	}
	// Only the hash is stored, and the proof is not derived from anything the session carries.
	for id, hash := range store.hashes {
		if strings.Contains(hash, one[identity.EnvProof]) || strings.Contains(one[identity.EnvProof], string(id)) {
			t.Fatalf("the stored value or the proof exposes %s", id)
		}
	}
}

// Every relaunch (restore, resume, a conversion) mints a new proof; the previous process's proof
// stops working the moment its replacement exists.
func TestRelaunchRotatesTheProofAndTheOldOneStopsWorking(t *testing.T) {
	store := &memoryProofStore{hashes: map[domain.SessionID]string{}}
	resolver := identity.NewResolver(store, liveSessions{"mer-1": {ID: "mer-1", ProjectID: "mer", Kind: domain.KindWorker}})
	manager := proofManager(identity.NewIssuer(store, nil))

	first, _, err := manager.launchRuntimeEnv("mer-1", "mer", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := manager.launchRuntimeEnv("mer-1", "mer", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if first[identity.EnvProof] == second[identity.EnvProof] {
		t.Fatal("a relaunch reused the previous proof")
	}
	if _, err := resolver.Resolve(context.Background(), first[identity.EnvProof]); !errors.Is(err, identity.ErrInvalid) {
		t.Fatalf("the previous launch's proof = %v, want invalid after a relaunch", err)
	}
	if c, err := resolver.Resolve(context.Background(), second[identity.EnvProof]); err != nil || c.Session != "mer-1" {
		t.Fatalf("the current launch's proof = %+v, %v", c, err)
	}
}

// A session that cannot be given an identity must not start: it would run with no way to tell
// the daemon who it is, and the daemon would (rightly) refuse everything it asked.
func TestLaunchFailsWhenTheProofCannotBeIssued(t *testing.T) {
	env, _, err := proofManager(failingProofIssuer{}).launchRuntimeEnv("mer-1", "mer", "", nil)
	if err == nil || env != nil {
		t.Fatalf("launch env = %v, %v; want a refusal", env, err)
	}
	if !strings.Contains(err.Error(), "identity proof") {
		t.Fatalf("error %q does not say what failed", err)
	}
}

// Neither a project nor the daemon's own environment can supply a proof; an environment that is
// built for anything other than a launch (a preflight, a cleanup, a bridge) carries none.
func TestOnlyALaunchCarriesAProof(t *testing.T) {
	manager := proofManager(identity.NewIssuer(&memoryProofStore{hashes: map[domain.SessionID]string{}}, nil))
	t.Setenv(identity.EnvProof, "aop1.from-the-daemons-own-environment")
	env := manager.runtimeEnv("mer-1", "mer", "", map[string]string{identity.EnvProof: "aop1.from-the-project", "AO_SESSION_ID": "someone-else"})
	if env[identity.EnvProof] != "" {
		t.Fatalf("a non-launch environment carries a proof: %q", env[identity.EnvProof])
	}
	if env[EnvSessionID] != "mer-1" {
		t.Fatalf("AO_SESSION_ID = %q, want the launched session", env[EnvSessionID])
	}
	launch, _, err := manager.launchRuntimeEnv("mer-1", "mer", "", map[string]string{identity.EnvProof: "aop1.from-the-project"})
	if err != nil {
		t.Fatal(err)
	}
	if got := launch[identity.EnvProof]; got == "" || got == "aop1.from-the-project" || got == "aop1.from-the-daemons-own-environment" {
		t.Fatalf("a launch's proof = %q, want one the daemon minted", got)
	}
}

func TestNoIssuerMeansNoProof(t *testing.T) {
	env, _, err := proofManager(nil).launchRuntimeEnv("mer-1", "mer", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if env[identity.EnvProof] != "" {
		t.Fatalf("a manager without an identity issuer injected %q", env[identity.EnvProof])
	}
}

// --- ED8-1 review F2/F3: a proof is issued exactly when a launch environment is built ---

// The rule (docs in package identity): a session's proof changes only when a NEW process is
// launched for it. Anything that builds an environment without launching (a preflight, a probe,
// a cleanup, an interface-handoff check) must not call launchRuntimeEnv, or it would replace the
// proof of a session whose process is alive and silently break every ao call that process makes.
// launchRuntimeEnv is therefore reachable only from the two launch-environment builders.
func TestOnlyLaunchEnvironmentBuildersIssueAProof(t *testing.T) {
	allowed := map[string]bool{"prepareWorkerLaunchEnv": true, "prepareChatControllerEnv": true}
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }, 0)
	if err != nil {
		t.Fatal(err)
	}
	var offenders []string
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil || fn.Name.Name == "launchRuntimeEnv" {
					continue
				}
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "launchRuntimeEnv" && !allowed[fn.Name.Name] {
						offenders = append(offenders, fn.Name.Name+" ("+fset.Position(call.Pos()).String()+")")
					}
					return true
				})
			}
		}
	}
	if len(offenders) != 0 {
		t.Fatalf("launchRuntimeEnv (which issues a proof) is called from %v; only %v may call it. Build a probe environment with runtimeEnv instead", offenders, []string{"prepareWorkerLaunchEnv", "prepareChatControllerEnv"})
	}
}

// Probing whether a stopped terminal could resume natively (the update-when-idle check) builds
// an environment but launches nothing, so the session's proof must not change.
func TestProbingATerminalDoesNotRotateItsProof(t *testing.T) {
	store := &memoryProofStore{hashes: map[domain.SessionID]string{}}
	mgr, st, rt := newChatManager(&recordingLauncher{})
	mgr.proofs = identity.NewIssuer(store, nil)
	mgr.runtime = idleWorkloadRuntime{rt}
	st.sessions["resumable"] = domain.SessionRecord{ID: "resumable", ProjectID: chatTestProject, Kind: domain.KindWorker,
		Harness: domain.HarnessCodex, Mode: domain.SessionModeTUI, Activity: domain.Activity{State: domain.ActivityIdle},
		Metadata: domain.SessionMetadata{WorkspacePath: "/ws/resumable", Branch: "ao/resumable", RuntimeHandleID: "pane-resumable", AgentSessionID: "native-1"}}
	issued, err := identity.NewIssuer(store, nil).Issue(context.Background(), "resumable")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := mgr.BusyHarnessSessions(context.Background(), domain.HarnessCodex); err != nil {
		t.Fatal(err)
	}
	if got := store.hashes["resumable"]; got != identity.Hash(issued) {
		t.Fatal("a resume probe replaced the proof of a session whose process is still running")
	}
}

// A restore launches a new process: its environment carries a proof that was issued while the
// environment was built, so it is the session's current proof (and the previous one is dead).
func TestRestoreLaunchesWithTheSessionsCurrentProof(t *testing.T) {
	store := &memoryProofStore{hashes: map[domain.SessionID]string{}}
	m, st, rt, _ := newManager()
	m.proofs = identity.NewIssuer(store, nil)
	seedTerminal(st, "mer-1", domain.SessionMetadata{WorkspacePath: "/ws/mer-1", Branch: "b", AgentSessionID: "agent-x"})
	old, err := identity.NewIssuer(store, nil).Issue(context.Background(), "mer-1")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := m.RestoreWithMode(context.Background(), "mer-1"); err != nil {
		t.Fatal(err)
	}
	proof := rt.lastCfg.Env[identity.EnvProof]
	if proof == "" {
		t.Fatal("the restored process was started without a proof")
	}
	if store.hashes["mer-1"] != identity.Hash(proof) {
		t.Fatal("the proof in the restored environment is not the one the daemon holds for the session")
	}
	if proof == old {
		t.Fatal("a restore reused the previous process's proof")
	}
}
