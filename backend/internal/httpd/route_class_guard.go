package httpd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/requestscope"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/routeclass"
	"github.com/aoagents/agent-orchestrator/backend/internal/identity"
)

// RouteClassMode says how strictly identity (class A) is applied.
type RouteClassMode int

const (
	// RouteClassOff applies no identity decision (the zero value: embedders and tests that mount the
	// router without an identity setup). Classification still applies: an unclassified mutating
	// route is refused.
	RouteClassOff RouteClassMode = iota
	// RouteClassObserve is the daemon's default: an anonymous caller passes the A-class routes whose
	// Observe flag allows it (ed.7 needed no identity there) and is counted; the predicate and
	// authority routes still refuse it. A mutating route with NO class is refused regardless.
	RouteClassObserve
	// RouteClassEnforce refuses every anonymous A-class call.
	RouteClassEnforce
)

// GuardConfig configures the route-class guard.
type GuardConfig struct {
	// EnforceHuman makes class H and HP routes require a verified human capability. The
	// daemon sets it (ed.8's impersonation fix); embedders and tests without the app's key leave
	// it off and H is not applied.
	EnforceHuman bool
	// Identity is how strictly class A is applied (observe by default; the flip to enforce is its
	// own reviewed build).
	Identity RouteClassMode
	// Coordinator reports who the active coordinator is and whether it holds a proof, for the
	// coordinator-only routes' two phases. Nil is treated as "unknown": the strict phase.
	Coordinator CoordinatorProofs
	// Reproof, when set, is asked to put an un-proven session at the front of the idle re-proof
	// queue when it is refused.
	Reproof Prioritizer
	// Observer, when set, is told about every anonymous call on a classified route.
	Observer Observer
	// bridgedCoordinator remembers, for this daemon run, the coordinator session the bridge has
	// identified on a coordinator-only route. Set by routeClassGuard.
	bridgedCoordinator *atomic.Value
}

// CoordinatorProofs reports the active coordinator's proof state, looked up per request so a
// replaced or relaunched coordinator is seen at once.
type CoordinatorProofs interface {
	CoordinatorState(ctx context.Context) (identity.CoordinatorState, error)
}

// Prioritizer moves an un-proven session to the front of the idle re-proof queue. It returns
// true when the session is a live session without a proof that is now queued (so the refusal can
// say it will be relaunched), false for an unknown or already proven session.
type Prioritizer interface {
	Prioritize(ctx context.Context, id domain.SessionID) bool
}

// AnonymousCall is one request from a caller that proved nothing, on a classified route.
type AnonymousCall struct {
	Method, Route string
	Class         routeclass.Class
	// Claimed is the session the caller says it is (X-AO-Session-Claim); never an identity.
	Claimed domain.SessionID
	Allowed bool
	// Reason is the refusal code, empty when allowed.
	Reason string
	// ClientPort and ServerPort identify the loopback connection (0 when not loopback), so the
	// observations can name the calling process best-effort.
	ClientPort, ServerPort int
}

// Observer receives AnonymousCall events. It must not block the request.
type Observer interface {
	ObserveAnonymous(AnonymousCall)
}

// Refusal codes.
const (
	codeRouteUnclassified   = "ROUTE_UNCLASSIFIED"
	codeHumanRequired       = "HUMAN_CAPABILITY_REQUIRED"
	codeCallerIdentityNeeds = "CALLER_IDENTITY_REQUIRED"
	codeSessionNeedsIdent   = "SESSION_NEEDS_IDENTITY"
	codeCoordinatorIdent    = "COORDINATOR_IDENTITY_REQUIRED"
)

// maxKindPeek bounds the request body the guard reads to find a request's kind.
const maxKindPeek = 1 << 20

// routeClassGuard is deny-by-default for mutating routes (ED8). For the matched route's class:
//
//   - a mutating route with NO class is refused, always (nothing new may ship unguarded; a test
//     walks the live router so this cannot happen in a build);
//   - H and HP need the person (a verified human capability) when EnforceHuman is on;
//   - A needs the person or a verified agent or reviewer. For an anonymous caller the route's
//     fixed Observe flag decides in observe mode (ok: pass and count; kinds: pass only listed
//     request kinds; until-coordinator-proof: pass only while no coordinator holds a proof;
//     none: refuse), and enforce mode refuses. The coordinator-only routes require the
//     coordinator's own identity from the moment it holds a proof, in both modes;
//   - O, LC, T and OwnProof are decided by the route's own mechanism;
//   - LAN-listener requests are left to the LAN listener's own authentication.
//
// Reads need no class, except the terminal upgrade (GET /mux), which can type into any terminal.
// The route's own predicate (self, owner, manager, reviewer of that run) is the handler's job.
func routeClassGuard(router chi.Routes, cfg GuardConfig, log *slog.Logger) func(http.Handler) http.Handler {
	cfg.bridgedCoordinator = &atomic.Value{}
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
			switch entry.Class {
			case routeclass.H, routeclass.HP:
				if !cfg.EnforceHuman {
					break
				}
				if !caller.IsHuman() {
					cfg.noteAnonymous(r, entry, pattern, caller, false, codeHumanRequired)
					envelope.WriteAPIError(w, r, http.StatusForbidden, "forbidden", codeHumanRequired, refusalMessage(codeHumanRequired), nil)
					return
				}
			case routeclass.A:
				if cfg.Identity == RouteClassOff {
					break
				}
				if code, refuse := cfg.classARefusal(r, entry, pattern, caller, log); refuse {
					msg := refusalMessage(code)
					envelope.WriteAPIError(w, r, http.StatusForbidden, "forbidden", code, msg, nil)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// classARefusal decides an A-class request: whether it is refused, with which code. It also
// records anonymous calls and asks for the claiming session's re-proof when refusing it.
//
// A caller recognised only by process ancestry (a pre-upgrade session, ED8 D8) is NOT an identified
// agent here: it is treated as anonymous, and passes only what an anonymous caller may (the
// ObserveOK routes and the listed kinds), in every mode, because that is the bridge's whole
// purpose; and the coordinator-only routes only when it is the active coordinator in the first
// phase. Everything with authority (merge, review submit, kill, spawn of orchestrators, ...) is
// ObserveNone and is refused for it.
func (cfg GuardConfig) classARefusal(r *http.Request, entry routeclass.Entry, pattern string, caller identity.Caller, log *slog.Logger) (string, bool) {
	if caller.IsHuman() {
		return "", false
	}
	if entry.Observe == routeclass.ObserveUntilCoordinatorProof {
		// Phase 2: once the coordinator holds a proof (or any orchestrator of its project ever
		// did), only its own proven identity passes (anonymous, bridged, another agent and a
		// reviewer are refused). If the state cannot be read, the strict phase applies.
		state, err := identity.CoordinatorState{}, error(nil)
		if cfg.Coordinator != nil {
			state, err = cfg.Coordinator.CoordinatorState(r.Context())
		}
		if cfg.Coordinator == nil || err != nil || state.HoldsProof || state.EverProven {
			if cfg.Coordinator != nil && err != nil {
				log.Warn("coordinator proof state unavailable; treating the coordinator as proven", "error", err)
			}
			if state.Session != "" && caller.IsSession(state.Session) {
				return "", false
			}
			cfg.noteAnonymous(r, entry, pattern, caller, false, codeCoordinatorIdent)
			return codeCoordinatorIdent, true
		}
		// Phase 1: no coordinator proof yet. Identified callers pass as in ed.7; a caller known
		// only by ancestry passes as the active coordinator and no other session; anonymous is
		// observe-allowed (below).
		if caller.IsBridged() {
			if state.Session != "" && caller.Session == state.Session {
				cfg.bridgedCoordinator.Store(state.Session)
				cfg.noteAnonymous(r, entry, pattern, caller, true, "")
				return "", false
			}
			cfg.noteAnonymous(r, entry, pattern, caller, false, codeCallerIdentityNeeds)
			return codeCallerIdentityNeeds, true
		}
		// Once the bridge has identified the active coordinator, a caller that proved nothing and
		// matched no session is not that coordinator: refused, as after a proof.
		if !caller.IsAgent() && !caller.IsReviewer() && state.Session != "" {
			if seen, _ := cfg.bridgedCoordinator.Load().(domain.SessionID); seen == state.Session {
				cfg.noteAnonymous(r, entry, pattern, caller, false, codeCoordinatorIdent)
				return codeCoordinatorIdent, true
			}
		}
	}
	if caller.IsAgent() || caller.IsReviewer() {
		return "", false
	}
	// An anonymous or bridged caller. Enforce mode refuses the anonymous; a bridged caller is the
	// pre-upgrade session the bridge exists for and is held to the same per-route list.
	if cfg.Identity == RouteClassEnforce && !caller.IsBridged() {
		return cfg.refuseAnonymous(r, entry, pattern, caller), true
	}
	allowed := false
	switch entry.Observe {
	case routeclass.ObserveOK, routeclass.ObserveUntilCoordinatorProof:
		allowed = true
	case routeclass.ObserveKinds:
		allowed = kindAllowed(r, entry)
	}
	if allowed {
		cfg.noteAnonymous(r, entry, pattern, caller, true, "")
		return "", false
	}
	return cfg.refuseAnonymous(r, entry, pattern, caller), true
}

// refuseAnonymous records the refusal and returns its code: SESSION_NEEDS_IDENTITY (and a
// prioritized re-proof of that session) when the caller claims a live un-proven session, else
// CALLER_IDENTITY_REQUIRED.
func (cfg GuardConfig) refuseAnonymous(r *http.Request, entry routeclass.Entry, pattern string, caller identity.Caller) string {
	code := codeCallerIdentityNeeds
	if claimed := claimOf(r, caller); claimed != "" && cfg.Reproof != nil && cfg.Reproof.Prioritize(r.Context(), claimed) {
		code = codeSessionNeedsIdent
	}
	cfg.noteAnonymous(r, entry, pattern, caller, false, code)
	return code
}

func (cfg GuardConfig) noteAnonymous(r *http.Request, entry routeclass.Entry, pattern string, caller identity.Caller, allowed bool, reason string) {
	if cfg.Observer == nil || caller.IsHuman() || caller.IsAgent() || caller.IsReviewer() {
		return
	}
	client, server, _ := loopbackPorts(r)
	cfg.Observer.ObserveAnonymous(AnonymousCall{
		Method: r.Method, Route: pattern, Class: entry.Class, Claimed: claimOf(r, caller),
		Allowed: allowed, Reason: reason, ClientPort: client, ServerPort: server,
	})
}

// claimOf is the session an anonymous call is attributed to: for a caller the daemon matched by
// process ancestry, the matched session (from the kernel, not the request); otherwise the session
// the request CLAIMS, which is a diagnostic and never an identity.
func claimOf(r *http.Request, caller identity.Caller) domain.SessionID {
	if caller.IsBridged() {
		return caller.Session
	}
	return claimedSession(r)
}

// claimedSession is the session the caller says it is (a claim, never an identity).
func claimedSession(r *http.Request) domain.SessionID {
	id := strings.TrimSpace(r.Header.Get(identity.HeaderClaim))
	if id == "" || len(id) > 128 {
		return ""
	}
	return domain.SessionID(id)
}

// kindAllowed reports whether the request body's "kind" is one the route's Observe list allows.
// It reads the body (bounded) and puts it back for the handler; an unreadable, oversize or
// kind-less body is not allowed.
func kindAllowed(r *http.Request, entry routeclass.Entry) bool {
	if r.Body == nil {
		return slices.Contains(entry.Kinds, "")
	}
	original := r.Body
	head, err := io.ReadAll(io.LimitReader(original, maxKindPeek+1))
	if err != nil || len(head) > maxKindPeek {
		r.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(head), original), original}
		return false
	}
	_ = original.Close()
	r.Body = io.NopCloser(bytes.NewReader(head))
	var probe struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(head, &probe); err != nil {
		return false
	}
	return slices.Contains(entry.Kinds, probe.Kind)
}

func refusalMessage(code string) string {
	switch code {
	case codeHumanRequired:
		return "Only the person using the desktop app can do this."
	case codeSessionNeedsIdent:
		return "This session has no identity proof yet; it will be relaunched automatically when idle. Retry in a few minutes."
	case codeCoordinatorIdent:
		return "The coordinator now has an identity proof; run this from the coordinator session."
	}
	return "This needs an identified caller: run it from an AO session, or from the app."
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

// ParseRouteClassMode reads the guard mode from AO_ROUTE_CLASS_MODE: "enforce" turns class
// enforcement on; anything else (including unset) is observe, the default. Unclassified mutating
// routes are refused in both.
func ParseRouteClassMode(value string) RouteClassMode {
	if strings.EqualFold(strings.TrimSpace(value), "enforce") {
		return RouteClassEnforce
	}
	return RouteClassObserve
}
