package httpd

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/requestscope"
	"github.com/aoagents/agent-orchestrator/backend/internal/identity"
)

type proofStore struct{ hashes map[domain.SessionID]string }

func (p *proofStore) SetSessionProofHash(_ context.Context, id domain.SessionID, hash string, _ time.Time) error {
	p.hashes[id] = hash
	return nil
}

func (p *proofStore) SessionIdentityByProofHash(_ context.Context, hash string) (domain.SessionID, string, bool, error) {
	for id, h := range p.hashes {
		if h == hash {
			return id, h, true, nil
		}
	}
	return "", "", false, nil
}

func (p *proofStore) DeleteSessionProof(_ context.Context, id domain.SessionID) error {
	delete(p.hashes, id)
	return nil
}

type proofSessions map[domain.SessionID]domain.SessionRecord

func (p proofSessions) GetSession(_ context.Context, id domain.SessionID) (domain.SessionRecord, bool, error) {
	rec, ok := p[id]
	return rec, ok, nil
}

// callerFor sends one request through the identity middleware and returns who the handler saw.
func callerFor(t *testing.T, resolver *identity.Resolver, mark func(*http.Request) *http.Request, proof string) (identity.Caller, int) {
	t.Helper()
	var seen identity.Caller
	h := callerIdentityMiddleware(resolver, slog.New(slog.NewTextHandler(io.Discard, nil)))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = identity.FromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/worker-1/send", nil)
	if proof != "" {
		req.Header.Set(identity.HeaderProof, proof)
	}
	if mark != nil {
		req = mark(req)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return seen, rec.Code
}

func TestCallerIdentityResolvesWhoIsCallingAndNeverRefuses(t *testing.T) {
	store := &proofStore{hashes: map[domain.SessionID]string{}}
	sessions := proofSessions{
		"worker-1":  {ID: "worker-1", ProjectID: "repo", Kind: domain.KindWorker},
		"manager-1": {ID: "manager-1", ProjectID: "repo", Kind: domain.KindOrchestrator},
		"gone-1":    {ID: "gone-1", ProjectID: "repo", Kind: domain.KindWorker, IsTerminated: true},
	}
	issuer := identity.NewIssuer(store, nil)
	resolver := identity.NewResolver(store, sessions)
	ctx := context.Background()
	worker, _ := issuer.Issue(ctx, "worker-1")
	manager, _ := issuer.Issue(ctx, "manager-1")
	old, _ := issuer.Issue(ctx, "worker-1") // a relaunch: worker's earlier proof is now stale
	worker, old = old, worker
	ended, _ := issuer.Issue(ctx, "gone-1")
	unknown, _ := identity.NewProof()
	human := func(r *http.Request) *http.Request { return r.WithContext(requestscope.WithHumanUI(r.Context())) }
	lan := func(r *http.Request) *http.Request { return r.WithContext(requestscope.WithLAN(r.Context())) }
	lanHuman := func(r *http.Request) *http.Request { return lan(human(r)) }

	cases := []struct {
		name  string
		proof string
		mark  func(*http.Request) *http.Request
		check func(identity.Caller) bool
	}{
		{"no proof, no capability: anonymous", "", nil, func(c identity.Caller) bool { return c.Kind == identity.KindAnonymous }},
		{"a worker's proof: that worker, from the daemon's record", worker, nil, func(c identity.Caller) bool {
			return c.IsAgent() && c.Session == "worker-1" && c.SessionKind == domain.KindWorker && c.Project == "repo"
		}},
		{"a manager's proof: that manager", manager, nil, func(c identity.Caller) bool {
			return c.IsAgent() && c.Session == "manager-1" && c.SessionKind == domain.KindOrchestrator
		}},
		{"a proof the session no longer holds: anonymous", old, nil, func(c identity.Caller) bool { return c.Kind == identity.KindAnonymous }},
		{"a proof that was never issued: anonymous", unknown, nil, func(c identity.Caller) bool { return c.Kind == identity.KindAnonymous }},
		{"a session id in place of a proof: anonymous", "worker-1", nil, func(c identity.Caller) bool { return c.Kind == identity.KindAnonymous }},
		{"an ended session's proof: anonymous", ended, nil, func(c identity.Caller) bool { return c.Kind == identity.KindAnonymous }},
		{"a verified human capability: the human", "", human, func(c identity.Caller) bool { return c.IsHuman() && c.Session == "" }},
		{"a human capability beats a proof", worker, human, func(c identity.Caller) bool { return c.IsHuman() }},
		{"a LAN request is never identified by a proof", worker, lan, func(c identity.Caller) bool { return c.Kind == identity.KindAnonymous }},
		{"nor by a capability", "", lanHuman, func(c identity.Caller) bool { return c.Kind == identity.KindAnonymous }},
	}
	for _, tc := range cases {
		got, status := callerFor(t, resolver, tc.mark, tc.proof)
		if status != http.StatusNoContent {
			t.Errorf("%s: the middleware refused the request (%d); it must only identify", tc.name, status)
		}
		if !tc.check(got) {
			t.Errorf("%s: caller = %+v", tc.name, got)
		}
	}
}

func TestWithoutAResolverOnlyTheHumanCanBeIdentified(t *testing.T) {
	proof, _ := identity.NewProof()
	if c, _ := callerFor(t, nil, nil, proof); c.Kind != identity.KindAnonymous {
		t.Fatalf("a daemon with no identity resolver identified %+v", c)
	}
	human := func(r *http.Request) *http.Request { return r.WithContext(requestscope.WithHumanUI(r.Context())) }
	if c, _ := callerFor(t, nil, human, ""); !c.IsHuman() {
		t.Fatalf("the human was not identified without a resolver: %+v", c)
	}
}
