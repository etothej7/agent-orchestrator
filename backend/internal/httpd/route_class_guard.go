package httpd

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/requestscope"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/routeclass"
	"github.com/aoagents/agent-orchestrator/backend/internal/identity"
)

// RouteClassMode says how strictly the route-class guard applies a route's class.
type RouteClassMode int

const (
	// RouteClassObserve is the default: every classified route is let through and a request its
	// class would have refused is only logged (debug), so the app, the CLI and phones keep working
	// while the app signs every human action. A mutating route with NO class is still refused.
	RouteClassObserve RouteClassMode = iota
	// RouteClassEnforce refuses a request whose caller does not satisfy its route's class.
	RouteClassEnforce
)

// Refusal codes.
const (
	codeRouteUnclassified   = "ROUTE_UNCLASSIFIED"
	codeHumanRequired       = "HUMAN_CAPABILITY_REQUIRED"
	codeCallerIdentityNeeds = "CALLER_IDENTITY_REQUIRED"
)

// routeClassGuard is deny-by-default for mutating routes (ED8). It looks up the matched route's
// class in routeclass, and:
//
//   - refuses a mutating route that has no class, always: nothing new may ship unguarded (a test
//     walks the live router so this cannot happen in a build);
//   - enforces the class in RouteClassEnforce mode: H and HP need the person (a verified human
//     capability), A needs the person or a verified agent or reviewer, and O, LC, T and OwnProof
//     are decided by the route's own mechanism (open, loopback control, the session capability
//     headers, the credential in the request);
//   - leaves LAN-listener requests to the LAN listener's own authentication: the phone's
//     bearer credential cannot present a local capability, and which routes a phone may use as
//     the person is a separate decision.
//
// Reads are not mutating and need no class, except the terminal upgrade (GET /mux), which can
// type into any terminal. The route's predicate (self, owner, manager, reviewer of that run) is
// the handler's job.
func routeClassGuard(router chi.Routes, mode RouteClassMode, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !needsClass(r) || requestscope.IsLAN(r.Context()) {
				next.ServeHTTP(w, r)
				return
			}
			rctx := chi.NewRouteContext()
			if !router.Match(rctx, r.Method, routingPath(r)) {
				next.ServeHTTP(w, r) // no such route: the router's own 404/405
				return
			}
			pattern := rctx.RoutePattern()
			entry, classified := routeclass.Lookup(r.Method, pattern)
			if !classified {
				log.Warn("refusing a mutating route with no class", "method", r.Method, "route", pattern)
				envelope.WriteAPIError(w, r, http.StatusForbidden, "forbidden", codeRouteUnclassified,
					"This route has no caller class, so it cannot be used.", nil)
				return
			}
			caller := identity.FromContext(r.Context())
			if code, refused := classRefusal(entry.Class, caller); refused {
				if mode == RouteClassEnforce {
					envelope.WriteAPIError(w, r, http.StatusForbidden, "forbidden", code, refusalMessage(code), nil)
					return
				}
				log.Debug("route class would refuse", "method", r.Method, "route", pattern, "class", string(entry.Class), "caller", string(caller.Kind), "code", code)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// routingPath is the path chi routes on: the escaped path when the request has one (an encoded
// slash must not change which route matches), else the decoded path. The guard must classify the
// exact route the router will pick, or an encoded path could slip past its class.
func routingPath(r *http.Request) string {
	if r.URL.RawPath != "" {
		return r.URL.RawPath
	}
	return r.URL.Path
}

// needsClass reports whether a request is one the guard classifies: every mutating method, and
// the terminal upgrade.
func needsClass(r *http.Request) bool {
	return routeclass.IsWrite(r.Method) || (r.Method == http.MethodGet && r.URL.Path == "/mux")
}

// classRefusal says whether a caller fails a route's class, and with which code.
func classRefusal(class routeclass.Class, c identity.Caller) (code string, refused bool) {
	switch class {
	case routeclass.H, routeclass.HP:
		return codeHumanRequired, !c.IsHuman()
	case routeclass.A:
		return codeCallerIdentityNeeds, !c.IsHuman() && !c.IsAgent() && !c.IsReviewer()
	}
	return "", false
}

func refusalMessage(code string) string {
	if code == codeHumanRequired {
		return "Only the person using the desktop app can do this."
	}
	return "This needs an identified caller: run it from an AO session, or from the app."
}

// ParseRouteClassMode reads the guard mode from AO_ROUTE_CLASS_MODE: "enforce" turns class
// enforcement on; anything else (including unset) is observe, the default. Unclassified mutating
// routes are refused in both.
func ParseRouteClassMode(value string) RouteClassMode {
	if strings.EqualFold(strings.TrimSpace(value), "enforce") {
		return RouteClassEnforce
	}
	return RouteClassObserve
}
