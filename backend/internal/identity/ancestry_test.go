package identity

import (
	"context"
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/procidentity"
)

type fakeAncestry struct {
	res procidentity.Result
	err error
}

func (f fakeAncestry) Identify(context.Context, int, int) (procidentity.Result, error) {
	return f.res, f.err
}

// proofCounts is a store that also reports which sessions have a CONFIRMED proof (gen > 0).
type proofCounts struct {
	*fakeStore
	gen map[domain.SessionID]int
}

func (p proofCounts) SessionProofConfirmed(_ context.Context, id domain.SessionID) (bool, error) {
	return p.gen[id] > 0, nil
}

func ancestryResolver(res procidentity.Result, err error, gen map[domain.SessionID]int) *Resolver {
	store := proofCounts{fakeStore: newFakeStore(), gen: gen}
	sessions := fakeSessions{
		"worker-1": {ID: "worker-1", ProjectID: "repo", Kind: domain.KindWorker},
		"gone-1":   {ID: "gone-1", ProjectID: "repo", Kind: domain.KindWorker, IsTerminated: true},
	}
	return NewResolver(store, sessions).WithAncestry(fakeAncestry{res: res, err: err})
}

// The bridge: a live session that has never been given a proof is recognised through its process
// tree. The identity comes from the daemon's session record; it is marked as ancestry-derived.
func TestABridgedSessionResolvesFromItsRecordAndIsMarkedAsAncestry(t *testing.T) {
	r := ancestryResolver(procidentity.Result{Session: "worker-1", PID: 4242, Comm: "ao"}, nil, map[domain.SessionID]int{})
	c, err := r.ResolveByAncestry(context.Background(), 50000, 3001)
	if err != nil || !c.IsBridged() || c.IsAgent() || c.Session != "worker-1" || c.SessionKind != domain.KindWorker || c.Project != "repo" || c.Via != ViaAncestry {
		t.Fatalf("caller = %+v, %v; want worker-1 (worker, repo) via ancestry", c, err)
	}
}

// The bridge ends at the session's first relaunch: once it holds a proof, an ancestry match is
// not an identity any more (a proof-less caller inside a proven session is just anonymous).
func TestTheBridgeEndsWhenTheSessionHasAProof(t *testing.T) {
	r := ancestryResolver(procidentity.Result{Session: "worker-1"}, nil, map[domain.SessionID]int{"worker-1": 1})
	if c, err := r.ResolveByAncestry(context.Background(), 1, 2); err == nil || c.Kind != KindAnonymous {
		t.Fatalf("a proven session was identified by ancestry: %+v, %v", c, err)
	}
}

func TestTheBridgeFailsClosed(t *testing.T) {
	boom := errors.New("sysctl failed")
	for name, r := range map[string]*Resolver{
		"the lookup failed":      ancestryResolver(procidentity.Result{}, boom, nil),
		"the session has ended":  ancestryResolver(procidentity.Result{Session: "gone-1"}, nil, map[domain.SessionID]int{}),
		"the session is unknown": ancestryResolver(procidentity.Result{Session: "nobody"}, nil, map[domain.SessionID]int{}),
		"no ancestry is wired":   NewResolver(newFakeStore(), fakeSessions{"worker-1": {ID: "worker-1"}}),
		"the store cannot count": NewResolver(newFakeStore(), fakeSessions{"worker-1": {ID: "worker-1", ProjectID: "repo"}}).WithAncestry(fakeAncestry{res: procidentity.Result{Session: "worker-1"}}),
	} {
		if c, err := r.ResolveByAncestry(context.Background(), 1, 2); err == nil || c.Kind != KindAnonymous {
			t.Errorf("%s: caller = %+v, %v; want anonymous and an error", name, c, err)
		}
	}
}

// A proof's identity says how it was established.
func TestAProofIdentityIsMarkedAsProof(t *testing.T) {
	_, issuer, resolver, _ := fixture()
	proof, _ := issuer.Issue(context.Background(), "worker-1")
	c, err := resolver.Resolve(context.Background(), proof)
	if err != nil || c.Via != ViaProof {
		t.Fatalf("caller = %+v, %v; want via proof", c, err)
	}
}
