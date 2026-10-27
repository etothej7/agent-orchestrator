package httpd

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/requestscope"
	"github.com/aoagents/agent-orchestrator/backend/internal/identity"
)

// guardedRouter mounts a handful of routes of each class (plus one nobody classified) behind the
// real guard, with the caller chosen per request by the test.
func guardedRouter(mode RouteClassMode, caller *identity.Caller) (chi.Router, *int) {
	reached := 0
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { reached++; w.WriteHeader(http.StatusNoContent) })
	r := chi.NewRouter()
	// the caller is set by the test the way callerIdentityMiddleware sets it in production
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(identity.WithCaller(req.Context(), *caller)))
		})
	})
	r.Use(routeClassGuard(r, mode, slog.New(slog.NewTextHandler(io.Discard, nil))))
	r.Post("/api/v1/mobile/enable", ok)                            // H
	r.Put("/api/v1/sessions/{sessionId}/auto-resume", ok)          // HP
	r.Post("/api/v1/sessions/{sessionId}/send", ok)                // A
	r.Post("/api/v1/reviews/{reviewId}/conversation/messages", ok) // H
	r.Delete("/api/v1/sessions/{sessionId}/preview/app/*", ok)     // T
	r.Post("/api/v1/push/devices", ok)                             // O
	r.Post("/shutdown", ok)                                        // LC
	r.Post("/api/v1/remote-host/account-token", ok)                // OwnProof
	r.Post("/api/v1/brand-new-write-route", ok)                    // not in the table
	r.Get("/api/v1/sessions", ok)                                  // read: no class needed
	r.Get("/mux", ok)                                              // H: a write-capable upgrade
	return r, &reached
}

func call(r http.Handler, method, path string, mark func(*http.Request) *http.Request) int {
	req := httptest.NewRequest(method, path, strings.NewReader("{}"))
	if mark != nil {
		req = mark(req)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec.Code
}

func TestRouteClassGuardEnforcesTheClassOfTheRoute(t *testing.T) {
	human, anonymous := identity.Human(), identity.Anonymous()
	agent := identity.Caller{Kind: identity.KindAgent, Session: "worker-1", SessionKind: domain.KindWorker, Project: "repo"}
	reviewer := identity.Caller{Kind: identity.KindReviewer, Review: "review-1", Worker: "worker-1", Project: "repo"}
	type route struct{ method, path string }
	h := route{"POST", "/api/v1/mobile/enable"}
	hp := route{"PUT", "/api/v1/sessions/worker-1/auto-resume"}
	a := route{"POST", "/api/v1/sessions/worker-1/send"}
	tt := route{"DELETE", "/api/v1/sessions/worker-1/preview/app/x"}
	o := route{"POST", "/api/v1/push/devices"}
	lc := route{"POST", "/shutdown"}
	own := route{"POST", "/api/v1/remote-host/account-token"}
	mux := route{"GET", "/mux"}

	cases := []struct {
		name   string
		caller identity.Caller
		route  route
		want   int
	}{
		{"person on a human-only route", human, h, http.StatusNoContent},
		{"agent on a human-only route", agent, h, http.StatusForbidden},
		{"reviewer on a human-only route", reviewer, h, http.StatusForbidden},
		{"nobody on a human-only route", anonymous, h, http.StatusForbidden},
		{"agent on a human-or-pending route", agent, hp, http.StatusForbidden},
		{"person on a human-or-pending route", human, hp, http.StatusNoContent},
		{"person on an agent route (the UI is never refused)", human, a, http.StatusNoContent},
		{"agent on an agent route", agent, a, http.StatusNoContent},
		{"reviewer on an agent route", reviewer, a, http.StatusNoContent},
		{"nobody on an agent route", anonymous, a, http.StatusForbidden},
		{"nobody on a session-bound route is left to its own capability", anonymous, tt, http.StatusNoContent},
		{"nobody on an open route", anonymous, o, http.StatusNoContent},
		{"nobody on local control", anonymous, lc, http.StatusNoContent},
		{"nobody on an own-proof route", anonymous, own, http.StatusNoContent},
		{"nobody on the terminal upgrade", anonymous, mux, http.StatusForbidden},
		{"person on the terminal upgrade", human, mux, http.StatusNoContent},
	}
	for _, tc := range cases {
		caller := tc.caller
		router, _ := guardedRouter(RouteClassEnforce, &caller)
		if got := call(router, tc.route.method, tc.route.path, nil); got != tc.want {
			t.Errorf("%s: status = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// Deny by default: a mutating route nobody classified is refused for everyone, in every mode.
func TestRouteClassGuardDeniesAnUnclassifiedWriteRoute(t *testing.T) {
	for _, mode := range []RouteClassMode{RouteClassObserve, RouteClassEnforce} {
		for _, who := range []identity.Caller{identity.Human(), identity.Anonymous(), {Kind: identity.KindAgent, Session: "worker-1"}} {
			caller := who
			router, reached := guardedRouter(mode, &caller)
			if got := call(router, "POST", "/api/v1/brand-new-write-route", nil); got != http.StatusForbidden || *reached != 0 {
				t.Errorf("mode %v, caller %v: status %d, handler reached %d times; an unclassified write route must be refused", mode, who.Kind, got, *reached)
			}
		}
	}
}

// Observe (the default until the renderer signs every human action and the terminal question is
// decided) refuses nothing that has a class: it only reports.
func TestRouteClassGuardObserveModeNeverRefusesAClassifiedRoute(t *testing.T) {
	caller := identity.Anonymous()
	router, reached := guardedRouter(RouteClassObserve, &caller)
	for _, p := range []struct{ method, path string }{
		{"POST", "/api/v1/mobile/enable"}, {"POST", "/api/v1/sessions/worker-1/send"}, {"GET", "/mux"},
	} {
		if got := call(router, p.method, p.path, nil); got != http.StatusNoContent {
			t.Errorf("observe mode refused %s %s (%d)", p.method, p.path, got)
		}
	}
	if *reached != 3 {
		t.Fatalf("handlers reached = %d, want all 3", *reached)
	}
}

// Reads and unmatched paths are not the guard's business.
func TestRouteClassGuardLeavesReadsAndUnknownPathsToTheRouter(t *testing.T) {
	caller := identity.Anonymous()
	router, _ := guardedRouter(RouteClassEnforce, &caller)
	if got := call(router, "GET", "/api/v1/sessions", nil); got != http.StatusNoContent {
		t.Errorf("a read was refused (%d)", got)
	}
	if got := call(router, "POST", "/api/v1/no/such/route", nil); got != http.StatusNotFound {
		t.Errorf("an unknown path = %d, want the router's 404", got)
	}
}

// LAN requests carry the phone's bearer credential and are classified by the LAN listener's own
// rules (the LAN-human subset is a separate decision); the class guard leaves them alone.
func TestRouteClassGuardLeavesLANRequestsToTheLANListener(t *testing.T) {
	caller := identity.Anonymous()
	router, _ := guardedRouter(RouteClassEnforce, &caller)
	lan := func(r *http.Request) *http.Request { return r.WithContext(requestscope.WithLAN(r.Context())) }
	if got := call(router, "POST", "/api/v1/sessions/worker-1/send", lan); got != http.StatusNoContent {
		t.Errorf("a LAN request was refused by the class guard (%d)", got)
	}
}

func TestRouteClassGuardRefusalsCarryAStableCode(t *testing.T) {
	caller := identity.Anonymous()
	router, _ := guardedRouter(RouteClassEnforce, &caller)
	for path, code := range map[string]string{
		"/api/v1/mobile/enable":          "HUMAN_CAPABILITY_REQUIRED",
		"/api/v1/sessions/worker-1/send": "CALLER_IDENTITY_REQUIRED",
		"/api/v1/brand-new-write-route":  "ROUTE_UNCLASSIFIED",
	} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), code) {
			t.Errorf("%s: %d %q, want 403 with %s", path, rec.Code, rec.Body.String(), code)
		}
	}
}

// The guard classifies the route chi will actually pick, including for a path with encoded
// characters (chi routes on the escaped path), so an encoded request cannot dodge its class.
func TestRouteClassGuardClassifiesEncodedPathsLikeTheRouter(t *testing.T) {
	caller := identity.Anonymous()
	router, reached := guardedRouter(RouteClassEnforce, &caller)
	for _, path := range []string{
		"/api/v1/sessions/worker%2F1/send",
		"/api/v1/sessions/worker%201/send",
		"/api/v1/mobile%2Fenable", // not the mobile route: must be the router's business, never an unguarded handler
	} {
		before := *reached
		got := call(router, "POST", path, nil)
		if *reached != before {
			t.Errorf("%s reached a handler for an anonymous caller (status %d)", path, got)
		}
	}
}

// The same guard on the REAL router: an agent-less local caller cannot enable mobile access
// (human-only) once classes are enforced, and an unclassified write cannot exist.
func TestRealRouterEnforcesTheMobileRoutesAreHumanOnly(t *testing.T) {
	router := NewRouterWithControl(config.Config{}, nil, nil, APIDeps{
		Mobile:         &controllers.MobileController{Bridge: fakeMobileBridge{}},
		RouteClassMode: RouteClassEnforce,
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
