package identity

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

type fakeStore struct {
	hashes map[domain.SessionID]string
	failOn string
}

func newFakeStore() *fakeStore { return &fakeStore{hashes: map[domain.SessionID]string{}} }

func (f *fakeStore) SetSessionProofHash(_ context.Context, id domain.SessionID, hash string, _ time.Time) error {
	f.hashes[id] = hash
	return nil
}

func (f *fakeStore) SessionIdentityByProofHash(_ context.Context, hash string) (domain.SessionID, string, bool, error) {
	if f.failOn == "lookup" {
		return "", "", false, errors.New("disk on fire")
	}
	for id, h := range f.hashes {
		if h == hash {
			return id, h, true, nil
		}
	}
	return "", "", false, nil
}

func (f *fakeStore) DeleteSessionProof(_ context.Context, id domain.SessionID) error {
	delete(f.hashes, id)
	return nil
}

type fakeSessions map[domain.SessionID]domain.SessionRecord

func (f fakeSessions) GetSession(_ context.Context, id domain.SessionID) (domain.SessionRecord, bool, error) {
	rec, ok := f[id]
	return rec, ok, nil
}

func fixture() (*fakeStore, *Issuer, *Resolver, fakeSessions) {
	store := newFakeStore()
	sessions := fakeSessions{
		"worker-1":  {ID: "worker-1", ProjectID: "repo", Kind: domain.KindWorker},
		"manager-1": {ID: "manager-1", ProjectID: "repo", Kind: domain.KindOrchestrator},
		"gone-1":    {ID: "gone-1", ProjectID: "repo", Kind: domain.KindWorker, IsTerminated: true},
	}
	return store, NewIssuer(store, nil), NewResolver(store, sessions), sessions
}

func TestProofsAreRandomWellFormedAndNeverDerivedFromTheSession(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 500; i++ {
		p, err := NewProof()
		if err != nil {
			t.Fatal(err)
		}
		if !WellFormed(p) || !strings.HasPrefix(p, "aop1.") {
			t.Fatalf("proof %q is not well formed", p)
		}
		if seen[p] {
			t.Fatal("two proofs were equal")
		}
		seen[p] = true
	}
	_, issuer, _, _ := fixture()
	a, _ := issuer.Issue(context.Background(), "worker-1")
	b, _ := issuer.Issue(context.Background(), "worker-1")
	if a == b {
		t.Fatal("issuing twice for one session produced the same proof: proofs must not be derived from the session")
	}
}

func TestWellFormedRefusesAnythingElse(t *testing.T) {
	good, _ := NewProof()
	for name, s := range map[string]string{
		"empty": "", "no prefix": strings.TrimPrefix(good, "aop1."), "wrong prefix": "aop2." + strings.TrimPrefix(good, "aop1."),
		"too short": "aop1.abc", "not base64": "aop1." + strings.Repeat("!", 43), "session id": "worker-1", "too long": good + "AAAA",
	} {
		if WellFormed(s) {
			t.Errorf("%s: %q accepted", name, s)
		}
	}
}

func TestHashIsHexSHA256AndOnlyTheHashIsStored(t *testing.T) {
	store, issuer, _, _ := fixture()
	proof, err := issuer.Issue(context.Background(), "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	if got := store.hashes["worker-1"]; got != Hash(proof) || len(got) != 64 || strings.Contains(got, proof) {
		t.Fatalf("stored %q for proof %q: want exactly the 64-char hash", got, proof)
	}
}

func TestResolveIdentifiesTheIssuedSessionFromTheDaemonsOwnRecord(t *testing.T) {
	_, issuer, resolver, _ := fixture()
	ctx := context.Background()
	workerProof, _ := issuer.Issue(ctx, "worker-1")
	managerProof, _ := issuer.Issue(ctx, "manager-1")

	w, err := resolver.Resolve(ctx, workerProof)
	if err != nil || !w.IsAgent() || w.Session != "worker-1" || w.SessionKind != domain.KindWorker || w.Project != "repo" {
		t.Fatalf("worker caller = %+v, %v", w, err)
	}
	m, err := resolver.Resolve(ctx, " "+managerProof+"\n")
	if err != nil || m.Session != "manager-1" || m.SessionKind != domain.KindOrchestrator {
		t.Fatalf("manager caller = %+v, %v", m, err)
	}
	// One session's proof never identifies another.
	if w.Session == m.Session {
		t.Fatal("two proofs resolved to the same session")
	}
}

func TestResolveRefusesEveryProofThatIsNotLive(t *testing.T) {
	store, issuer, resolver, _ := fixture()
	ctx := context.Background()
	old, _ := issuer.Issue(ctx, "worker-1")
	current, _ := issuer.Issue(ctx, "worker-1") // a relaunch rotates the proof
	unknown, _ := NewProof()
	ended, _ := issuer.Issue(ctx, "gone-1")

	for name, tc := range map[string]struct {
		proof string
		want  error
	}{
		"missing":                   {"", ErrMissing},
		"whitespace":                {"   ", ErrMissing},
		"malformed":                 {"worker-1", ErrInvalid},
		"a session id as a proof":   {"aop1.worker-1", ErrInvalid},
		"never issued":              {unknown, ErrInvalid},
		"replaced by a relaunch":    {old, ErrInvalid},
		"a terminated session":      {ended, ErrSessionEnded},
		"the right proof, tampered": {current[:len(current)-2] + "AA", ErrInvalid},
	} {
		c, err := resolver.Resolve(ctx, tc.proof)
		if !errors.Is(err, tc.want) || c.Kind != KindAnonymous {
			t.Errorf("%s: caller %+v err %v, want anonymous and %v", name, c, err, tc.want)
		}
	}
	if c, err := resolver.Resolve(ctx, current); err != nil || c.Session != "worker-1" {
		t.Fatalf("the current proof must still work: %+v, %v", c, err)
	}

	// A store error is an error, never a fall-through to someone.
	store.failOn = "lookup"
	if c, err := resolver.Resolve(ctx, current); err == nil || c.Kind != KindAnonymous {
		t.Fatalf("store failure = %+v, %v; want an error and anonymous", c, err)
	}
}

func TestRevokedProofStopsResolving(t *testing.T) {
	_, issuer, resolver, _ := fixture()
	ctx := context.Background()
	proof, _ := issuer.Issue(ctx, "worker-1")
	if err := issuer.Revoke(ctx, "worker-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.Resolve(ctx, proof); !errors.Is(err, ErrInvalid) {
		t.Fatalf("revoked proof = %v, want invalid", err)
	}
}

func TestAContextNothingVerifiedIsAnonymous(t *testing.T) {
	if c := FromContext(context.Background()); c.Kind != KindAnonymous || c.IsHuman() || c.IsAgent() {
		t.Fatalf("unmarked context caller = %+v", c)
	}
	ctx := WithCaller(context.Background(), Human())
	if !FromContext(ctx).IsHuman() {
		t.Fatal("a human caller was lost")
	}
	ctx = WithCaller(context.Background(), Caller{Kind: KindAgent, Session: "worker-1"})
	if c := FromContext(ctx); !c.IsAgent() || c.IsHuman() {
		t.Fatalf("agent caller = %+v", c)
	}
	// An agent kind with no session is not an agent: nothing identifies it.
	if (Caller{Kind: KindAgent}).IsAgent() {
		t.Fatal("an agent caller with no session id counted as an agent")
	}
}

func TestProofFromEnvPrefersTheEnvironmentThenAPrivateFile(t *testing.T) {
	proof, _ := NewProof()
	filed, _ := NewProof()
	dir := t.TempDir()
	write := func(name, body string, mode os.FileMode) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		return path
	}
	env := func(pairs ...string) func(string) string {
		m := map[string]string{}
		for i := 0; i < len(pairs); i += 2 {
			m[pairs[i]] = pairs[i+1]
		}
		return func(k string) string { return m[k] }
	}
	good := write("good", filed+"\n", 0o600)

	if got := ProofFromEnv(env(EnvProof, proof, EnvProofFile, good)); got != proof {
		t.Fatalf("env proof = %q, want the environment's", got)
	}
	if got := ProofFromEnv(env(EnvProofFile, good)); got != filed {
		t.Fatalf("file proof = %q, want the file's", got)
	}
	for name, getenv := range map[string]func(string) string{
		"nothing":                 env(),
		"a relative path":         env(EnvProofFile, "good"),
		"a missing file":          env(EnvProofFile, filepath.Join(dir, "missing")),
		"a world-readable file":   env(EnvProofFile, write("loose", filed, 0o644)),
		"a group-readable file":   env(EnvProofFile, write("group", filed, 0o640)),
		"a file that is not one":  env(EnvProofFile, write("junk", "not-a-proof", 0o600)),
		"a directory":             env(EnvProofFile, dir),
		"an oversized file":       env(EnvProofFile, write("big", strings.Repeat("a", 1000), 0o600)),
		"a symlink to a good one": env(EnvProofFile, func() string { l := filepath.Join(dir, "link"); _ = os.Symlink(good, l); return l }()),
	} {
		if got := ProofFromEnv(getenv); got != "" {
			t.Errorf("%s: returned %q, want no proof", name, got)
		}
	}
}

// confirmingStore records which proofs were presented, like the real store's two-phase rotation.
type confirmingStore struct {
	*fakeStore
	confirmed []string
}

func (c *confirmingStore) ConfirmSessionProof(_ context.Context, id domain.SessionID, hash string, _ time.Time) error {
	c.confirmed = append(c.confirmed, string(id)+":"+hash)
	return nil
}

// Presenting a valid proof confirms it (which is what promotes a pending successor in the store).
func TestAValidProofIsConfirmedWhenPresented(t *testing.T) {
	store := &confirmingStore{fakeStore: newFakeStore()}
	sessions := fakeSessions{"worker-1": {ID: "worker-1", ProjectID: "repo", Kind: domain.KindWorker}}
	issuer, resolver := NewIssuer(store, nil), NewResolver(store, sessions)
	proof, _ := issuer.Issue(context.Background(), "worker-1")
	if _, err := resolver.Resolve(context.Background(), proof); err != nil {
		t.Fatal(err)
	}
	if len(store.confirmed) != 1 || store.confirmed[0] != "worker-1:"+Hash(proof) {
		t.Fatalf("confirmed = %v, want the presented proof's hash for worker-1", store.confirmed)
	}
	// A refused proof confirms nothing.
	bad, _ := NewProof()
	_, _ = resolver.Resolve(context.Background(), bad)
	if len(store.confirmed) != 1 {
		t.Fatalf("a refused proof was confirmed: %v", store.confirmed)
	}
}
