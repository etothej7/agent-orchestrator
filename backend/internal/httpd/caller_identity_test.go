package httpd

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/requestscope"
	"github.com/aoagents/agent-orchestrator/backend/internal/identity"
	"github.com/aoagents/agent-orchestrator/backend/internal/procidentity"
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

// W14 (upgrade): a session that was already running when the identity-enforcing daemon started
// holds no proof. Everything it can still say about itself is a claim, and claims never identify:
// the caller is nobody, so it has no authority (and no grace window gives it any).
func TestASessionStartedBeforeTheUpgradeIsNobodyUntilItIsRelaunched(t *testing.T) {
	store := &proofStore{hashes: map[domain.SessionID]string{}} // no proofs issued yet
	sessions := proofSessions{
		"worker-1":  {ID: "worker-1", ProjectID: "repo", Kind: domain.KindWorker},
		"manager-1": {ID: "manager-1", ProjectID: "repo", Kind: domain.KindOrchestrator},
	}
	resolver := identity.NewResolver(store, sessions)

	var seen identity.Caller
	h := callerIdentityMiddleware(resolver, slog.New(slog.NewTextHandler(io.Discard, nil)))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = identity.FromContext(r.Context())
	}))
	claims := func(r *http.Request) {
		r.Header.Set("X-AO-Session-ID", "manager-1")
		r.Header.Set("Authorization", "Bearer manager-1")
		r.Header.Set("Cookie", "ao_session=manager-1")
		q := r.URL.Query()
		q.Set("senderSessionId", "manager-1")
		q.Set("sessionId", "manager-1")
		r.URL.RawQuery = q.Encode()
	}
	body := strings.NewReader(`{"senderSessionId":"manager-1","sessionId":"manager-1","origin":"human"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/manager-1/send", body)
	claims(req)
	h.ServeHTTP(httptest.NewRecorder(), req)

	if seen.Kind != identity.KindAnonymous || seen.Session != "" {
		t.Fatalf("a session with no proof, claiming to be manager-1 everywhere, was identified as %+v", seen)
	}
	// With no authority it can neither merge nor submit a review: it can only ask the person.
	if v := identity.DecideMerge(seen, true, identity.Target{Session: "worker-1", Project: "repo"}); v != identity.NeedsHuman {
		t.Fatalf("merge verdict for an unidentified caller = %v, want needs_human", v)
	}
	if identity.MaySubmitReview(seen, identity.ReviewRunRef{Review: "review-1", Worker: "worker-1"}) {
		t.Fatal("an unidentified caller may submit a review")
	}

	// The relaunch (resume) issues the proof; from then on the session is who it says.
	proof, err := identity.NewIssuer(store, nil).Issue(context.Background(), "manager-1")
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/v1/sessions/manager-1/send", nil)
	req.Header.Set(identity.HeaderProof, proof)
	h.ServeHTTP(httptest.NewRecorder(), req)
	if !seen.IsManagerOf("repo") {
		t.Fatalf("after the relaunch the manager is %+v", seen)
	}
}

// A reviewer is identified through the same middleware, as a reviewer and never as an agent;
// on the LAN listener it is nobody, like every proof.
func TestAReviewerProofIdentifiesAReviewerNotAnAgent(t *testing.T) {
	store := &proofStore{hashes: map[domain.SessionID]string{}}
	reviewStore := &reviewProofStore{hashes: map[string]string{}}
	sessions := proofSessions{"worker-1": {ID: "worker-1", ProjectID: "repo", Kind: domain.KindWorker}}
	reviews := reviewRows{"review-1": {ID: "review-1", SessionID: "worker-1", ProjectID: "repo"}}
	resolver := identity.NewResolver(store, sessions).WithReviews(reviewStore, reviews)
	proof, err := identity.NewIssuer(store, nil).WithReviews(reviewStore).IssueReview(context.Background(), "review-1")
	if err != nil {
		t.Fatal(err)
	}

	got, _ := callerFor(t, resolver, nil, proof)
	if !got.IsReviewer() || got.IsAgent() || got.Review != "review-1" || got.Worker != "worker-1" {
		t.Fatalf("caller = %+v, want the reviewer of review-1", got)
	}
	if !identity.MaySubmitReview(got, identity.ReviewRunRef{Review: "review-1", Worker: "worker-1"}) {
		t.Fatal("the reviewer may not submit its own run")
	}
	lan := func(r *http.Request) *http.Request { return r.WithContext(requestscope.WithLAN(r.Context())) }
	if onLAN, _ := callerFor(t, resolver, lan, proof); onLAN.Kind != identity.KindAnonymous {
		t.Fatalf("a reviewer proof on the LAN listener identified %+v", onLAN)
	}
}

type reviewProofStore struct{ hashes map[string]string }

func (r *reviewProofStore) SetReviewProofHash(_ context.Context, id, hash string, _ time.Time) error {
	r.hashes[id] = hash
	return nil
}

func (r *reviewProofStore) ReviewIdentityByProofHash(_ context.Context, hash string) (string, string, bool, error) {
	for id, h := range r.hashes {
		if h == hash {
			return id, h, true, nil
		}
	}
	return "", "", false, nil
}

func (r *reviewProofStore) DeleteReviewProof(_ context.Context, id string) error {
	delete(r.hashes, id)
	return nil
}

type reviewRows map[string]domain.Review

func (r reviewRows) GetReviewByID(_ context.Context, id string) (domain.Review, bool, error) {
	row, ok := r[id]
	return row, ok, nil
}

// --- the live-identity bridge (process ancestry) ---

func (p *proofStore) SessionProofGeneration(_ context.Context, id domain.SessionID) (int, error) {
	if _, ok := p.hashes[id]; ok {
		return 1, nil
	}
	return 0, nil
}

type countingAncestry struct {
	res   procidentity.Result
	err   error
	calls int
	ports [2]int
}

func (c *countingAncestry) Identify(_ context.Context, client, server int) (procidentity.Result, error) {
	c.calls++
	c.ports = [2]int{client, server}
	return c.res, c.err
}

// loopbackRequest is a request as the daemon's listener sees a local caller: a loopback peer on
// an ephemeral port, arriving at the daemon's port.
func loopbackRequest(t *testing.T, proof string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/worker-1/send", nil)
	req.RemoteAddr = "127.0.0.1:50321"
	req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 3001}))
	if proof != "" {
		req.Header.Set(identity.HeaderProof, proof)
	}
	return req
}

func seenCaller(resolver *identity.Resolver, req *http.Request) identity.Caller {
	var seen identity.Caller
	h := callerIdentityMiddleware(resolver, slog.New(slog.NewTextHandler(io.Discard, nil)))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = identity.FromContext(r.Context())
	}))
	h.ServeHTTP(httptest.NewRecorder(), req)
	return seen
}

func TestAProoflessLoopbackCallerInsideASessionTreeIsThatSession(t *testing.T) {
	store := &proofStore{hashes: map[domain.SessionID]string{}} // no proof issued: a pre-upgrade session
	sessions := proofSessions{"worker-1": {ID: "worker-1", ProjectID: "repo", Kind: domain.KindWorker}}
	ancestry := &countingAncestry{res: procidentity.Result{Session: "worker-1", PID: 4242, Comm: "ao"}}
	resolver := identity.NewResolver(store, sessions).WithAncestry(ancestry)

	got := seenCaller(resolver, loopbackRequest(t, ""))
	if !got.IsAgent() || got.Session != "worker-1" || got.Via != identity.ViaAncestry {
		t.Fatalf("caller = %+v, want worker-1 via ancestry", got)
	}
	if ancestry.ports != [2]int{50321, 3001} {
		t.Fatalf("ports used = %v, want the client's and the daemon's ports from the socket", ancestry.ports)
	}
}

// Ancestry is a last resort for a caller with NO proof. A proof that is presented but wrong
// never falls back to it; a human capability and a LAN request never consult it.
func TestAncestryIsOnlyConsultedForALoopbackCallerWithNoProofAndNoCapability(t *testing.T) {
	store := &proofStore{hashes: map[domain.SessionID]string{}}
	sessions := proofSessions{"worker-1": {ID: "worker-1", ProjectID: "repo", Kind: domain.KindWorker}}
	newResolver := func() (*identity.Resolver, *countingAncestry) {
		a := &countingAncestry{res: procidentity.Result{Session: "worker-1"}}
		return identity.NewResolver(store, sessions).WithAncestry(a), a
	}
	stale, _ := identity.NewProof()
	human := func(r *http.Request) *http.Request { return r.WithContext(requestscope.WithHumanUI(r.Context())) }
	lan := func(r *http.Request) *http.Request { return r.WithContext(requestscope.WithLAN(r.Context())) }
	remote := func(r *http.Request) *http.Request { r.RemoteAddr = "192.0.2.7:50321"; return r }

	for name, tc := range map[string]struct {
		req  func() *http.Request
		want func(identity.Caller) bool
	}{
		"a wrong proof does not fall back to ancestry": {func() *http.Request { return loopbackRequest(t, stale) }, func(c identity.Caller) bool { return c.Kind == identity.KindAnonymous }},
		"the human capability is not replaced":         {func() *http.Request { return human(loopbackRequest(t, "")) }, func(c identity.Caller) bool { return c.IsHuman() }},
		"a LAN request is never identified this way":   {func() *http.Request { return lan(loopbackRequest(t, "")) }, func(c identity.Caller) bool { return c.Kind == identity.KindAnonymous }},
		"a non-loopback peer is never identified":      {func() *http.Request { return remote(loopbackRequest(t, "")) }, func(c identity.Caller) bool { return c.Kind == identity.KindAnonymous }},
	} {
		resolver, ancestry := newResolver()
		if got := seenCaller(resolver, tc.req()); !tc.want(got) {
			t.Errorf("%s: caller = %+v", name, got)
		}
		if ancestry.calls != 0 {
			t.Errorf("%s: ancestry was consulted %d times", name, ancestry.calls)
		}
	}
}

func TestTheBridgeFailingLeavesTheCallerAnonymous(t *testing.T) {
	store := &proofStore{hashes: map[domain.SessionID]string{}}
	sessions := proofSessions{"worker-1": {ID: "worker-1", ProjectID: "repo", Kind: domain.KindWorker}}
	resolver := identity.NewResolver(store, sessions).WithAncestry(&countingAncestry{err: procidentity.ErrNotInSession})
	if got := seenCaller(resolver, loopbackRequest(t, "")); got.Kind != identity.KindAnonymous {
		t.Fatalf("caller = %+v, want anonymous", got)
	}
}
