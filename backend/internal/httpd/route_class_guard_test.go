package httpd

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/requestscope"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/routeclass"
	"github.com/aoagents/agent-orchestrator/backend/internal/identity"
)

type fakeCoordinator struct {
	state identity.CoordinatorState
	err   error
}

func (f fakeCoordinator) CoordinatorState(context.Context) (identity.CoordinatorState, error) {
	return f.state, f.err
}

type fakePrioritizer struct {
	mu     sync.Mutex
	asked  []domain.SessionID
	queued bool
}

func (f *fakePrioritizer) Prioritize(_ context.Context, id domain.SessionID) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, id)
	return f.queued
}

type recordingObserver struct {
	mu    sync.Mutex
	calls []AnonymousCall
}

func (r *recordingObserver) ObserveAnonymous(c AnonymousCall) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, c)
}

// guardFixture mounts routes of every class behind the real guard; the caller is set per request
// by the test the way callerIdentityMiddleware sets it in production.
type guardFixture struct {
	router  chi.Router
	reached *int
	obs     *recordingObserver
	reproof *fakePrioritizer
}

func newGuard(cfg GuardConfig, caller *identity.Caller) guardFixture {
	reached := 0
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { reached++; w.WriteHeader(http.StatusNoContent) })
	obs := &recordingObserver{}
	reproof := &fakePrioritizer{}
	if cfg.Observer == nil {
		cfg.Observer = obs
	}
	if cfg.Reproof == nil {
		cfg.Reproof = reproof
	}
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(identity.WithCaller(req.Context(), *caller)))
		})
	})
	r.Use(routeClassGuard(r, cfg, slog.New(slog.NewTextHandler(io.Discard, nil))))
	r.Post("/api/v1/mobile/enable", ok)                                             // H
	r.Put("/api/v1/sessions/{sessionId}/auto-resume", ok)                           // HP
	r.Post("/api/v1/sessions/{sessionId}/send", ok)                                 // A, observe OK
	r.Post("/api/v1/reviews/{reviewId}/conversation/messages", ok)                  // H
	r.Delete("/api/v1/sessions/{sessionId}/preview/app/*", ok)                      // T
	r.Post("/api/v1/push/devices", ok)                                              // O
	r.Post("/shutdown", ok)                                                         // LC
	r.Post("/api/v1/remote-host/account-token", ok)                                 // OwnProof
	r.Post("/api/v1/prs/{id}/merge", ok)                                            // A, never anonymous
	r.Post("/api/v1/sessions/{sessionId}/reviews/submit", ok)                       // A, never anonymous
	r.Post("/api/v1/sessions/{sessionId}/kill", ok)                                 // A, never anonymous
	r.Post("/api/v1/orchestrators", ok)                                             // A, never anonymous
	r.Post("/api/v1/sessions/{sessionId}/conversation/human-decisions", ok)         // coordinator-only
	r.Post("/api/v1/sessions/{sessionId}/conversation/human-decisions/resolve", ok) // coordinator-only
	r.Post("/api/v1/workspace/commands", ok)                                        // A, per kind
	r.Post("/api/v1/sessions", ok)                                                  // A, per kind
	r.Post("/api/v1/brand-new-write-route", ok)                                     // not in the table
	r.Get("/api/v1/sessions", ok)                                                   // read: no class needed
	r.Get("/mux", ok)                                                               // OwnProof: the ticket is the credential
	r.Put("/api/v1/projects/{id}", ok)                                              // HP: project settings
	r.Put("/api/v1/projects/{id}/config", ok)                                       // HP: project config
	r.Post("/api/v1/projects", ok)                                                  // HP: project add
	r.Post("/api/v1/dev/import-projects", ok)                                       // HP: project import
	return guardFixture{router: r, reached: &reached, obs: obs, reproof: reproof}
}

func do(r http.Handler, method, path, body string, mark func(*http.Request) *http.Request) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if mark != nil {
		req = mark(req)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func call(r http.Handler, method, path string, mark func(*http.Request) *http.Request) int {
	return do(r, method, path, "{}", mark).Code
}

var (
	human     = identity.Human()
	anonymous = identity.Anonymous()
	worker    = identity.Caller{Kind: identity.KindAgent, Session: "worker-1", SessionKind: domain.KindWorker, Project: "repo", Via: identity.ViaProof}
	manager   = identity.Caller{Kind: identity.KindAgent, Session: "manager-1", SessionKind: domain.KindOrchestrator, Project: "repo", Via: identity.ViaProof}
	coord     = identity.Caller{Kind: identity.KindAgent, Session: "coord-1", SessionKind: domain.KindOrchestrator, Project: "overall", Via: identity.ViaProof}
	reviewer  = identity.Caller{Kind: identity.KindReviewer, Review: "review-1", Worker: "worker-1", Project: "repo"}
)

// ---- class H: enforced for the person, regardless of the identity mode ----

func TestHumanOnlyRoutesNeedTheHumanCapability(t *testing.T) {
	type route struct{ method, path string }
	h := route{"POST", "/api/v1/mobile/enable"}
	hp := route{"PUT", "/api/v1/sessions/worker-1/auto-resume"}
	for _, mode := range []RouteClassMode{RouteClassObserve, RouteClassEnforce} {
		for name, tc := range map[string]struct {
			caller identity.Caller
			route  route
			want   int
		}{
			"person on a human-only route":       {human, h, http.StatusNoContent},
			"agent on a human-only route":        {worker, h, http.StatusForbidden},
			"reviewer on a human-only route":     {reviewer, h, http.StatusForbidden},
			"nobody on a human-only route":       {anonymous, h, http.StatusForbidden},
			"agent on a human-or-pending route":  {worker, hp, http.StatusForbidden},
			"person on a human-or-pending route": {human, hp, http.StatusNoContent},
		} {
			caller := tc.caller
			g := newGuard(GuardConfig{EnforceHuman: true, Identity: mode}, &caller)
			if got := call(g.router, tc.route.method, tc.route.path, nil); got != tc.want {
				t.Errorf("mode %v, %s: status = %d, want %d", mode, name, got, tc.want)
			}
		}
	}
}

// Without EnforceHuman (embedders and tests that do not hold the key) H is only reported.
func TestHumanEnforcementCanBeLeftOffForEmbedders(t *testing.T) {
	caller := anonymous
	g := newGuard(GuardConfig{}, &caller)
	if got := call(g.router, "POST", "/api/v1/mobile/enable", nil); got != http.StatusNoContent {
		t.Fatalf("with EnforceHuman off an H route was refused (%d)", got)
	}
}

// ---- class A: identity enforced or observed ----

func TestEnforceModeRefusesEveryAnonymousAClassCallWithAnActionableMessage(t *testing.T) {
	caller := anonymous
	g := newGuard(GuardConfig{EnforceHuman: true, Identity: RouteClassEnforce}, &caller)
	for _, p := range []string{"/api/v1/sessions/worker-1/send", "/api/v1/prs/7/merge", "/api/v1/sessions/worker-1/kill"} {
		rec := do(g.router, "POST", p, "{}", nil)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "CALLER_IDENTITY_REQUIRED") {
			t.Errorf("%s: %d %s, want 403 CALLER_IDENTITY_REQUIRED", p, rec.Code, rec.Body.String())
		}
	}
	for name, c := range map[string]identity.Caller{"worker": worker, "reviewer": reviewer, "person": human} {
		caller = c
		if got := call(g.router, "POST", "/api/v1/sessions/worker-1/send", nil); got != http.StatusNoContent {
			t.Errorf("%s was refused on an agent route in enforce mode (%d)", name, got)
		}
	}
}

// Guardrail 1: in observe mode an anonymous call passes ONLY where ed.7 needed no identity (and is
// counted); on the predicate routes it is refused in observe mode too.
func TestObserveModeNeverLetsAnonymousThroughAPredicateRoute(t *testing.T) {
	caller := anonymous
	g := newGuard(GuardConfig{EnforceHuman: true, Identity: RouteClassObserve}, &caller)
	if got := call(g.router, "POST", "/api/v1/sessions/worker-1/send", nil); got != http.StatusNoContent {
		t.Errorf("anonymous ao send was refused in observe mode (%d)", got)
	}
	for _, p := range []string{"/api/v1/prs/7/merge", "/api/v1/sessions/worker-1/reviews/submit", "/api/v1/sessions/worker-1/kill", "/api/v1/orchestrators"} {
		before := *g.reached
		if got := call(g.router, "POST", p, nil); got != http.StatusForbidden || *g.reached != before {
			t.Errorf("anonymous %s in observe mode: status %d, handler reached %v; a predicate route must refuse", p, got, *g.reached != before)
		}
	}
	if len(g.obs.calls) != 5 {
		t.Fatalf("observer saw %d anonymous calls, want all 5 (1 allowed, 4 refused)", len(g.obs.calls))
	}
	allowed := 0
	for _, c := range g.obs.calls {
		if c.Allowed {
			allowed++
		}
	}
	if allowed != 1 {
		t.Errorf("observer recorded %d allowed calls, want 1", allowed)
	}
}

// An identified agent passes an agent route in observe mode with no counting.
func TestIdentifiedCallersAreNotCounted(t *testing.T) {
	caller := worker
	g := newGuard(GuardConfig{Identity: RouteClassObserve, EnforceHuman: true}, &caller)
	if got := call(g.router, "POST", "/api/v1/sessions/worker-1/send", nil); got != http.StatusNoContent {
		t.Fatalf("status %d", got)
	}
	if len(g.obs.calls) != 0 {
		t.Fatalf("an identified caller was counted as anonymous: %+v", g.obs.calls)
	}
}

// Per-kind routes: story report kinds pass an anonymous caller in observe mode; the authority
// kinds and a missing or unreadable kind refuse it.
func TestObserveModeSplitsStoryCommandsAndSpawnByKind(t *testing.T) {
	caller := anonymous
	g := newGuard(GuardConfig{Identity: RouteClassObserve, EnforceHuman: true}, &caller)
	for name, tc := range map[string]struct {
		path, body string
		want       int
	}{
		"a worker's implementation evidence":  {"/api/v1/workspace/commands", `{"kind":"implementation","storyId":"s"}`, http.StatusNoContent},
		"a message":                           {"/api/v1/workspace/commands", `{"kind":"message"}`, http.StatusNoContent},
		"story done by its manager":           {"/api/v1/workspace/commands", `{"kind":"done"}`, http.StatusForbidden},
		"assignment":                          {"/api/v1/workspace/commands", `{"kind":"assignment"}`, http.StatusForbidden},
		"ready for approval":                  {"/api/v1/workspace/commands", `{"kind":"ready_for_approval"}`, http.StatusForbidden},
		"the board's answer":                  {"/api/v1/workspace/commands", `{"kind":"answer"}`, http.StatusForbidden},
		"an answer confirmation":              {"/api/v1/workspace/commands", `{"kind":"answer_confirmation"}`, http.StatusForbidden},
		"no kind (an empty-sender board act)": {"/api/v1/workspace/commands", `{"storyId":"s"}`, http.StatusForbidden},
		"an unreadable body":                  {"/api/v1/workspace/commands", `{not json`, http.StatusForbidden},
		"a worker spawn":                      {"/api/v1/sessions", `{"projectId":"repo","kind":"worker"}`, http.StatusNoContent},
		"a spawn with no kind (a worker)":     {"/api/v1/sessions", `{"projectId":"repo"}`, http.StatusNoContent},
		"spawning an orchestrator":            {"/api/v1/sessions", `{"projectId":"repo","kind":"orchestrator"}`, http.StatusForbidden},
	} {
		if got := do(g.router, "POST", tc.path, tc.body, nil).Code; got != tc.want {
			t.Errorf("%s: status %d, want %d", name, got, tc.want)
		}
	}
}

// The body the guard peeked at is still there for the handler.
func TestThePeekedBodyIsRestoredForTheHandler(t *testing.T) {
	caller := anonymous
	var got string
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(identity.WithCaller(req.Context(), caller)))
		})
	})
	r.Use(routeClassGuard(r, GuardConfig{Identity: RouteClassObserve, EnforceHuman: true}, slog.New(slog.NewTextHandler(io.Discard, nil))))
	r.Post("/api/v1/workspace/commands", func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		got = string(b)
		w.WriteHeader(http.StatusNoContent)
	})
	body := `{"kind":"message","text":"` + strings.Repeat("x", 5000) + `"}`
	if code := do(r, "POST", "/api/v1/workspace/commands", body, nil).Code; code != http.StatusNoContent || got != body {
		t.Fatalf("status %d, handler body intact: %v", code, got == body)
	}
}

// Routine action by an un-proven session: a refusal that says what is happening AND moves the
// session to the front of the re-proof queue; never a pending request.
func TestARoutineRefusalNamesTheSessionAndPrioritizesItsReproof(t *testing.T) {
	caller := anonymous
	g := newGuard(GuardConfig{Identity: RouteClassObserve, EnforceHuman: true}, &caller)
	g.reproof.queued = true
	claim := func(r *http.Request) *http.Request { r.Header.Set(identity.HeaderClaim, "manager-1"); return r }
	rec := do(g.router, "POST", "/api/v1/sessions/worker-1/kill", "{}", claim)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "SESSION_NEEDS_IDENTITY") || strings.Contains(rec.Body.String(), "pending") {
		t.Fatalf("%d %s, want 403 SESSION_NEEDS_IDENTITY with no pending request", rec.Code, rec.Body.String())
	}
	if len(g.reproof.asked) != 1 || g.reproof.asked[0] != "manager-1" {
		t.Fatalf("re-proof asked for %v, want manager-1", g.reproof.asked)
	}
	// A claim that names no live unproven session (a terminal) gets the plain refusal.
	g.reproof.queued = false
	rec = do(g.router, "POST", "/api/v1/sessions/worker-1/kill", "{}", claim)
	if !strings.Contains(rec.Body.String(), "CALLER_IDENTITY_REQUIRED") {
		t.Fatalf("%d %s, want CALLER_IDENTITY_REQUIRED", rec.Code, rec.Body.String())
	}
	// The claim is never an identity: the same call from a claimed manager with no proof stays refused.
	if len(g.obs.calls) == 0 || g.obs.calls[0].Claimed != "manager-1" {
		t.Fatalf("the claim was not recorded for the observations: %+v", g.obs.calls)
	}
}

// ---- the coordinator's two phases ----

func TestCoordinatorOnlyRoutesAreTwoPhase(t *testing.T) {
	routes := []string{
		"/api/v1/sessions/coord-1/conversation/human-decisions",
		"/api/v1/sessions/coord-1/conversation/human-decisions/resolve",
	}
	// Phase 1: the coordinator has NO proof.
	phase1 := GuardConfig{EnforceHuman: true, Identity: RouteClassObserve, Coordinator: fakeCoordinator{state: identity.CoordinatorState{Session: "coord-1", HoldsProof: false}}}
	// Phase 2: it has one (re-proofed, or relaunched).
	phase2 := GuardConfig{EnforceHuman: true, Identity: RouteClassObserve, Coordinator: fakeCoordinator{state: identity.CoordinatorState{Session: "coord-1", HoldsProof: true}}}

	for _, p := range routes {
		caller := anonymous
		g1 := newGuard(phase1, &caller)
		if got := call(g1.router, "POST", p, nil); got != http.StatusNoContent {
			t.Errorf("phase 1: anonymous %s refused (%d), want pass", p, got)
		}
		if len(g1.obs.calls) != 1 || !g1.obs.calls[0].Allowed {
			t.Errorf("phase 1: the anonymous call was not counted as allowed: %+v", g1.obs.calls)
		}

		caller = anonymous
		g2 := newGuard(phase2, &caller)
		rec := do(g2.router, "POST", p, "{}", nil)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "COORDINATOR_IDENTITY_REQUIRED") {
			t.Errorf("phase 2: anonymous %s = %d %s, want 403 COORDINATOR_IDENTITY_REQUIRED", p, rec.Code, rec.Body.String())
		}
		for name, c := range map[string]identity.Caller{"a worker's proof": worker, "a manager's proof": manager, "a reviewer": reviewer} {
			caller = c
			if got := call(g2.router, "POST", p, nil); got != http.StatusForbidden {
				t.Errorf("phase 2: %s passed %s (%d); only the coordinator may", name, p, got)
			}
		}
		caller = coord
		if got := call(g2.router, "POST", p, nil); got != http.StatusNoContent {
			t.Errorf("phase 2: the coordinator's own proof was refused on %s (%d)", p, got)
		}
	}
}

// The active coordinator is looked up per request, so a replaced coordinator cannot freeze the rule:
// a NEW coordinator with no proof reopens phase 1 for itself; the old session's proof no longer
// counts as the coordinator's.
func TestACoordinatorReplacementIsSeenOnTheNextRequest(t *testing.T) {
	state := &fakeCoordinatorBox{s: identity.CoordinatorState{Session: "coord-1", HoldsProof: true}}
	caller := coord
	g := newGuard(GuardConfig{Identity: RouteClassObserve, EnforceHuman: true, Coordinator: state}, &caller)
	p := "/api/v1/sessions/coord-2/conversation/human-decisions"
	if got := call(g.router, "POST", p, nil); got != http.StatusNoContent {
		t.Fatalf("coord-1 refused while it is the active coordinator (%d)", got)
	}
	state.set(identity.CoordinatorState{Session: "coord-2", HoldsProof: true}) // replaced and proven
	if got := call(g.router, "POST", p, nil); got != http.StatusForbidden {
		t.Fatalf("the replaced coordinator's proof still passed (%d)", got)
	}
}

type fakeCoordinatorBox struct {
	mu sync.Mutex
	s  identity.CoordinatorState
}

func (f *fakeCoordinatorBox) set(s identity.CoordinatorState) { f.mu.Lock(); f.s = s; f.mu.Unlock() }
func (f *fakeCoordinatorBox) CoordinatorState(context.Context) (identity.CoordinatorState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.s, nil
}

// If the daemon cannot tell whether the coordinator is proven, the strict phase applies.
func TestAnUnknownCoordinatorStateIsTreatedAsProven(t *testing.T) {
	caller := anonymous
	g := newGuard(GuardConfig{EnforceHuman: true, Identity: RouteClassObserve, Coordinator: fakeCoordinator{err: errors.New("db down")}}, &caller)
	if got := call(g.router, "POST", "/api/v1/sessions/coord-1/conversation/human-decisions", nil); got != http.StatusForbidden {
		t.Fatalf("anonymous passed while the coordinator state was unknown (%d)", got)
	}
}

// Identity off (the zero value, for embedders and tests): no identity decision on A routes, but
// classification still applies.
func TestIdentityOffAppliesNoIdentityDecisionButStillDeniesUnclassifiedRoutes(t *testing.T) {
	caller := anonymous
	g := newGuard(GuardConfig{}, &caller)
	for _, p := range []string{"/api/v1/prs/7/merge", "/api/v1/sessions/worker-1/kill", "/api/v1/sessions/coord-1/conversation/human-decisions"} {
		if got := call(g.router, "POST", p, nil); got != http.StatusNoContent {
			t.Errorf("identity off refused %s (%d)", p, got)
		}
	}
	if got := call(g.router, "POST", "/api/v1/brand-new-write-route", nil); got != http.StatusForbidden {
		t.Errorf("an unclassified route passed with identity off (%d)", got)
	}
	if len(g.obs.calls) != 0 {
		t.Errorf("identity off counted calls: %+v", g.obs.calls)
	}
}

// ---- unchanged guarantees ----

// Deny by default: a mutating route nobody classified is refused for everyone, in every mode.
func TestRouteClassGuardDeniesAnUnclassifiedWriteRoute(t *testing.T) {
	for _, mode := range []RouteClassMode{RouteClassOff, RouteClassObserve, RouteClassEnforce} {
		for _, who := range []identity.Caller{human, anonymous, worker} {
			caller := who
			g := newGuard(GuardConfig{Identity: mode}, &caller)
			if got := call(g.router, "POST", "/api/v1/brand-new-write-route", nil); got != http.StatusForbidden || *g.reached != 0 {
				t.Errorf("mode %v, caller %v: status %d, handler reached %d times; an unclassified write route must be refused", mode, who.Kind, got, *g.reached)
			}
		}
	}
}

func TestOpenAndSessionBoundRoutesAreLeftToTheirOwnMechanism(t *testing.T) {
	caller := anonymous
	g := newGuard(GuardConfig{EnforceHuman: true, Identity: RouteClassEnforce}, &caller)
	for _, p := range []struct{ method, path string }{
		{"DELETE", "/api/v1/sessions/worker-1/preview/app/x"}, {"POST", "/api/v1/push/devices"},
		{"POST", "/shutdown"}, {"POST", "/api/v1/remote-host/account-token"},
	} {
		if got := call(g.router, p.method, p.path, nil); got != http.StatusNoContent {
			t.Errorf("%s %s was refused (%d)", p.method, p.path, got)
		}
	}
}

func TestRouteClassGuardLeavesReadsAndUnknownPathsToTheRouter(t *testing.T) {
	caller := anonymous
	g := newGuard(GuardConfig{EnforceHuman: true, Identity: RouteClassEnforce}, &caller)
	if got := call(g.router, "GET", "/api/v1/sessions", nil); got != http.StatusNoContent {
		t.Errorf("a read was refused (%d)", got)
	}
	if got := call(g.router, "POST", "/api/v1/no/such/route", nil); got != http.StatusNotFound {
		t.Errorf("an unknown path = %d, want the router's 404", got)
	}
}

func TestRouteClassGuardLeavesLANRequestsToTheLANListener(t *testing.T) {
	caller := anonymous
	g := newGuard(GuardConfig{EnforceHuman: true, Identity: RouteClassEnforce}, &caller)
	lan := func(r *http.Request) *http.Request { return r.WithContext(requestscope.WithLAN(r.Context())) }
	if got := call(g.router, "POST", "/api/v1/sessions/worker-1/send", lan); got != http.StatusNoContent {
		t.Errorf("a LAN request was refused by the class guard (%d)", got)
	}
}

func TestRouteClassGuardRefusalsCarryAStableCode(t *testing.T) {
	caller := anonymous
	g := newGuard(GuardConfig{EnforceHuman: true, Identity: RouteClassEnforce}, &caller)
	for path, code := range map[string]string{
		"/api/v1/mobile/enable":          "HUMAN_CAPABILITY_REQUIRED",
		"/api/v1/sessions/worker-1/send": "CALLER_IDENTITY_REQUIRED",
		"/api/v1/brand-new-write-route":  "ROUTE_UNCLASSIFIED",
	} {
		rec := do(g.router, "POST", path, "{}", nil)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), code) {
			t.Errorf("%s: %d %q, want 403 with %s", path, rec.Code, rec.Body.String(), code)
		}
	}
}

// The guard classifies the route chi will actually pick, including for a path with encoded
// characters, so an encoded request cannot dodge its class.
func TestRouteClassGuardClassifiesEncodedPathsLikeTheRouter(t *testing.T) {
	caller := anonymous
	g := newGuard(GuardConfig{EnforceHuman: true, Identity: RouteClassEnforce}, &caller)
	for _, path := range []string{"/api/v1/sessions/worker%2F1/send", "/api/v1/sessions/worker%201/send", "/api/v1/mobile%2Fenable"} {
		before := *g.reached
		got := call(g.router, "POST", path, nil)
		if *g.reached != before {
			t.Errorf("%s reached a handler for an anonymous caller (status %d)", path, got)
		}
	}
}

// The same guard on the REAL router: human-only mobile routes are 403 without the capability.
func TestRealRouterEnforcesTheMobileRoutesAreHumanOnly(t *testing.T) {
	router := NewRouterWithControl(config.Config{}, nil, nil, APIDeps{
		Mobile:     &controllers.MobileController{Bridge: fakeMobileBridge{}},
		RouteGuard: GuardConfig{Identity: RouteClassObserve, EnforceHuman: true},
	}, ControlDeps{})
	for _, path := range []string{"/api/v1/mobile/enable", "/api/v1/mobile/regenerate", "/api/v1/mobile/disable", "/api/v1/mobile/secure-pairing"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "HUMAN_CAPABILITY_REQUIRED") {
			t.Errorf("%s without the capability: %d %s, want 403 HUMAN_CAPABILITY_REQUIRED", path, rec.Code, rec.Body.String())
		}
	}
}

func TestParseRouteClassMode(t *testing.T) {
	for in, want := range map[string]RouteClassMode{"": RouteClassObserve, "observe": RouteClassObserve, "enforce": RouteClassEnforce, " Enforce ": RouteClassEnforce, "yes": RouteClassObserve} {
		if got := ParseRouteClassMode(in); got != want {
			t.Errorf("ParseRouteClassMode(%q) = %v, want %v", in, got, want)
		}
	}
}

// ---- callers known only by process ancestry (ED8 D8): anonymous for every class-A decision ----

var (
	bridgedWorker  = identity.Caller{Kind: identity.KindAgent, Session: "worker-1", SessionKind: domain.KindWorker, Project: "repo", Via: identity.ViaAncestry}
	bridgedManager = identity.Caller{Kind: identity.KindAgent, Session: "manager-1", SessionKind: domain.KindOrchestrator, Project: "repo", Via: identity.ViaAncestry}
	bridgedCoord   = identity.Caller{Kind: identity.KindAgent, Session: "coord-1", SessionKind: domain.KindOrchestrator, Project: "overall", Via: identity.ViaAncestry}
)

func TestABridgedCallerIsRefusedOnEveryRouteWithAuthorityInEveryMode(t *testing.T) {
	for _, mode := range []RouteClassMode{RouteClassObserve, RouteClassEnforce} {
		for name, c := range map[string]identity.Caller{"worker": bridgedWorker, "manager": bridgedManager, "coordinator": bridgedCoord} {
			caller := c
			g := newGuard(GuardConfig{EnforceHuman: true, Identity: mode}, &caller)
			for _, p := range []string{"/api/v1/prs/7/merge", "/api/v1/sessions/worker-1/reviews/submit", "/api/v1/sessions/worker-1/kill", "/api/v1/orchestrators"} {
				before := *g.reached
				if got := call(g.router, "POST", p, nil); got != http.StatusForbidden || *g.reached != before {
					t.Errorf("mode %v: bridged %s on %s: status %d, handler reached %v; ancestry gives no authority", mode, name, p, got, *g.reached != before)
				}
			}
		}
	}
}

func TestABridgedCallerKeepsWorkingOnTheObserveOKRoutesAndIsCounted(t *testing.T) {
	for _, mode := range []RouteClassMode{RouteClassObserve, RouteClassEnforce} {
		caller := bridgedWorker
		g := newGuard(GuardConfig{EnforceHuman: true, Identity: mode}, &caller)
		if got := call(g.router, "POST", "/api/v1/sessions/worker-1/send", nil); got != http.StatusNoContent {
			t.Fatalf("mode %v: a pre-upgrade session's ao send was refused (%d)", mode, got)
		}
		if len(g.obs.calls) != 1 || !g.obs.calls[0].Allowed || g.obs.calls[0].Claimed != "worker-1" {
			t.Fatalf("mode %v: the bridged call was not counted against its session: %+v", mode, g.obs.calls)
		}
	}
}

func TestABridgedCallerGetsOnlyTheListedKindsOfAPerKindRoute(t *testing.T) {
	caller := bridgedWorker
	g := newGuard(GuardConfig{EnforceHuman: true, Identity: RouteClassEnforce}, &caller)
	if got := do(g.router, "POST", "/api/v1/sessions", `{"kind":"worker"}`, nil).Code; got != http.StatusNoContent {
		t.Fatalf("a bridged manager's worker spawn was refused (%d): the same list an anonymous caller gets", got)
	}
	if got := do(g.router, "POST", "/api/v1/sessions", `{"kind":"orchestrator"}`, nil).Code; got != http.StatusForbidden {
		t.Fatalf("a bridged caller spawned an orchestrator (%d)", got)
	}
	if got := do(g.router, "POST", "/api/v1/sessions", `not json`, nil).Code; got != http.StatusForbidden {
		t.Fatalf("a bridged caller with an unreadable body was not refused (%d)", got)
	}
}

func TestABridgedCoordinatorPassesTheCoordinatorRoutesOnlyInPhaseOne(t *testing.T) {
	p := "/api/v1/sessions/coord-1/conversation/human-decisions"
	state := &fakeCoordinatorBox{s: identity.CoordinatorState{Session: "coord-1"}}
	caller := bridgedCoord
	g := newGuard(GuardConfig{EnforceHuman: true, Identity: RouteClassEnforce, Coordinator: state}, &caller)
	if got := call(g.router, "POST", p, nil); got != http.StatusNoContent {
		t.Fatalf("the active coordinator (bridged) was refused in phase 1 (%d)", got)
	}
	for name, c := range map[string]identity.Caller{"worker": bridgedWorker, "manager": bridgedManager} {
		caller = c
		if got := call(g.router, "POST", p, nil); got != http.StatusForbidden {
			t.Errorf("a bridged %s passed a coordinator-only route in phase 1 (%d)", name, got)
		}
	}
	caller = bridgedCoord
	state.set(identity.CoordinatorState{Session: "coord-1", HoldsProof: true})
	if got := call(g.router, "POST", p, nil); got != http.StatusForbidden {
		t.Fatalf("a bridged coordinator passed in phase 2 (%d)", got)
	}
}

// The first phase never returns: once any orchestrator of the coordinator project ever held a
// proof, a moment with no active coordinator is still the strict phase.
func TestTheCoordinatorPhaseIsMonotonic(t *testing.T) {
	p := "/api/v1/sessions/coord-1/conversation/human-decisions"
	caller := anonymous
	g := newGuard(GuardConfig{EnforceHuman: true, Identity: RouteClassObserve,
		Coordinator: fakeCoordinator{state: identity.CoordinatorState{EverProven: true}}}, &caller)
	if got := call(g.router, "POST", p, nil); got != http.StatusForbidden {
		t.Fatalf("anonymous passed with no active coordinator after one had a proof (%d)", got)
	}
	caller = bridgedCoord
	if got := call(g.router, "POST", p, nil); got != http.StatusForbidden {
		t.Fatalf("a bridged caller passed with no active coordinator (%d)", got)
	}
	// never proven and no coordinator: phase 1, as before
	caller = anonymous
	g = newGuard(GuardConfig{EnforceHuman: true, Identity: RouteClassObserve, Coordinator: fakeCoordinator{}}, &caller)
	if got := call(g.router, "POST", p, nil); got != http.StatusNoContent {
		t.Fatalf("anonymous refused in phase 1 (%d)", got)
	}
}

// A refused bridged caller is told its session will be relaunched, using the session the KERNEL
// matched, not a header.
func TestARefusedBridgedCallerIsPrioritizedByItsMatchedSession(t *testing.T) {
	caller := bridgedWorker
	rp := &fakePrioritizer{queued: true}
	g := newGuard(GuardConfig{EnforceHuman: true, Identity: RouteClassObserve, Reproof: rp}, &caller)
	rec := do(g.router, "POST", "/api/v1/sessions/worker-1/kill", "{}", func(r *http.Request) *http.Request {
		r.Header.Set(identity.HeaderClaim, "some-other-session")
		return r
	})
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "SESSION_NEEDS_IDENTITY") {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
	if len(rp.asked) != 1 || rp.asked[0] != "worker-1" {
		t.Fatalf("prioritized %v, want the kernel-matched worker-1", rp.asked)
	}
}

// Once the bridge has identified the active coordinator on a coordinator-only route, a caller that
// proved nothing and matched no session is refused there, even though the coordinator holds no
// proof yet; before that, anonymous callers still pass (phase 1).
func TestOnceTheCoordinatorIsIdentifiedByAncestryAnonymousCallersAreRefused(t *testing.T) {
	p := "/api/v1/sessions/coord-1/conversation/human-decisions"
	state := &fakeCoordinatorBox{s: identity.CoordinatorState{Session: "coord-1"}}
	caller := anonymous
	g := newGuard(GuardConfig{EnforceHuman: true, Identity: RouteClassObserve, Coordinator: state}, &caller)
	if got := call(g.router, "POST", p, nil); got != http.StatusNoContent {
		t.Fatalf("anonymous refused before the coordinator was identified (%d)", got)
	}
	caller = bridgedCoord
	if got := call(g.router, "POST", p, nil); got != http.StatusNoContent {
		t.Fatalf("the bridged coordinator was refused (%d)", got)
	}
	caller = anonymous
	rec := do(g.router, "POST", p, "{}", nil)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "COORDINATOR_IDENTITY_REQUIRED") {
		t.Fatalf("anonymous after the coordinator was identified = %d %s, want 403 COORDINATOR_IDENTITY_REQUIRED", rec.Code, rec.Body.String())
	}
	// the memory is about THAT coordinator: a replacement that was not identified reopens phase 1 for itself
	state.set(identity.CoordinatorState{Session: "coord-2"})
	if got := call(g.router, "POST", "/api/v1/sessions/coord-2/conversation/human-decisions", nil); got != http.StatusNoContent {
		t.Fatalf("anonymous refused for a different, unidentified coordinator (%d)", got)
	}
	// a proven agent (the new coordinator after its relaunch) is unaffected by the memory
	caller = worker
	state.set(identity.CoordinatorState{Session: "coord-1"})
	if got := call(g.router, "POST", p, nil); got != http.StatusNoContent {
		t.Fatalf("a proven agent was refused in phase 1 (%d)", got)
	}
}

// agentMayMerge (and every other permission-widening project setting) is the person's: with the
// person-only guard on, an agent, a pre-upgrade session known only by ancestry, a reviewer and an
// anonymous caller each fail to write it through EVERY route that carries a project configuration,
// in both identity modes, and the handler (the only thing that could store it) never runs. Only the
// person's request reaches it.
func TestNoNonHumanCallerCanWriteProjectConfigThroughAnyRoute(t *testing.T) {
	type route struct{ method, path string }
	body := `{"config":{"agentMayMerge":true}}`
	routes := []route{{"PUT", "/api/v1/projects/repo"}, {"PUT", "/api/v1/projects/repo/config"}, {"POST", "/api/v1/projects"}, {"POST", "/api/v1/dev/import-projects"}}
	for _, mode := range []RouteClassMode{RouteClassObserve, RouteClassEnforce} {
		for name, c := range map[string]identity.Caller{"agent": worker, "manager": manager, "coordinator": coord, "bridged agent": bridgedWorker, "reviewer": reviewer, "anonymous": anonymous} {
			caller := c
			g := newGuard(GuardConfig{EnforceHuman: true, Identity: mode}, &caller)
			for _, rt := range routes {
				before := *g.reached
				rec := do(g.router, rt.method, rt.path, body, nil)
				if rec.Code != http.StatusForbidden || *g.reached != before {
					t.Errorf("mode %v: %s %s %s %s: status %d, handler reached %v; the project config is person-only", mode, name, rt.method, rt.path, body, rec.Code, *g.reached != before)
				}
			}
		}
		caller := human
		g := newGuard(GuardConfig{EnforceHuman: true, Identity: mode}, &caller)
		for _, rt := range routes {
			if got := do(g.router, rt.method, rt.path, body, nil).Code; got != http.StatusNoContent {
				t.Errorf("mode %v: the person was refused %s %s (%d)", mode, rt.method, rt.path, got)
			}
		}
	}
}

// GET /mux is OwnProof: the single-use ticket (minted by the class-H POST /terminal-tickets and
// redeemed by the upgrade handler) is its credential, so the guard decides nothing for any caller;
// pinning it to H would refuse the app's own terminals, which cannot sign a WebSocket upgrade.
func TestTheTerminalUpgradeIsLeftToItsOwnTicket(t *testing.T) {
	for _, mode := range []RouteClassMode{RouteClassObserve, RouteClassEnforce} {
		for name, c := range map[string]identity.Caller{"person": human, "anonymous": anonymous, "agent": worker, "bridged": bridgedWorker} {
			caller := c
			g := newGuard(GuardConfig{EnforceHuman: true, Identity: mode}, &caller)
			if got := call(g.router, "GET", "/mux", nil); got != http.StatusNoContent {
				t.Errorf("mode %v: %s on GET /mux = %d, want the guard to leave it to the ticket check", mode, name, got)
			}
		}
	}
	if e, ok := routeclass.Lookup("GET", "/mux"); !ok || e.Class != routeclass.OwnProof {
		t.Fatalf("GET /mux is %+v, want class OwnProof", e)
	}
	if e, ok := routeclass.Lookup("POST", "/api/v1/terminal-tickets"); ok && e.Class != routeclass.H {
		t.Fatalf("POST /terminal-tickets is %q, want H: only the person may mint a ticket", e.Class)
	}
}

// A refusal on a coordinator-only route promises the same prioritized idle re-proof as any other
// routine refusal: in the strict phase and after the bridge identified the coordinator, the session
// the unproven caller is attributed to is moved to the front of the queue. Proven callers are not.
func TestCoordinatorRouteRefusalsPrioritizeTheCallersSession(t *testing.T) {
	p := "/api/v1/sessions/coord-1/conversation/human-decisions"
	claim := func(r *http.Request) *http.Request { r.Header.Set(identity.HeaderClaim, "coord-1"); return r }

	// strict phase: the coordinator holds a proof; an unproven caller claiming coord-1 is refused and queued
	caller := anonymous
	rp := &fakePrioritizer{queued: true}
	g := newGuard(GuardConfig{EnforceHuman: true, Identity: RouteClassObserve, Reproof: rp,
		Coordinator: fakeCoordinator{state: identity.CoordinatorState{Session: "coord-1", HoldsProof: true}}}, &caller)
	rec := do(g.router, "POST", p, "{}", claim)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "COORDINATOR_IDENTITY_REQUIRED") {
		t.Fatalf("strict phase: %d %s", rec.Code, rec.Body.String())
	}
	if len(rp.asked) != 1 || rp.asked[0] != "coord-1" {
		t.Fatalf("strict phase: prioritized %v, want coord-1", rp.asked)
	}
	// a proven worker is refused but never "claims" anything
	caller = worker
	do(g.router, "POST", p, "{}", claim)
	if len(rp.asked) != 1 {
		t.Fatalf("a proven caller was prioritized: %v", rp.asked)
	}

	// phase 1 after the bridge identified the coordinator: an unproven, unmatched caller is refused and queued
	state := &fakeCoordinatorBox{s: identity.CoordinatorState{Session: "coord-1"}}
	rp2 := &fakePrioritizer{queued: true}
	caller = bridgedCoord
	g2 := newGuard(GuardConfig{EnforceHuman: true, Identity: RouteClassObserve, Reproof: rp2, Coordinator: state}, &caller)
	if got := call(g2.router, "POST", p, nil); got != http.StatusNoContent {
		t.Fatalf("bridged coordinator refused (%d)", got)
	}
	caller = anonymous
	if rec := do(g2.router, "POST", p, "{}", claim); rec.Code != http.StatusForbidden {
		t.Fatalf("phase 1 after identification: %d", rec.Code)
	}
	if len(rp2.asked) != 1 || rp2.asked[0] != "coord-1" {
		t.Fatalf("phase 1: prioritized %v, want coord-1", rp2.asked)
	}
}
