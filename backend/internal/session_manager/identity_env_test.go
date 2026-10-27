package sessionmanager

import (
	"context"
	"errors"
	"io"
	"log/slog"
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
