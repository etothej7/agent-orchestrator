package httpd

import (
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/requestscope"
	"github.com/aoagents/agent-orchestrator/backend/internal/identity"
)

// callerIdentityMiddleware resolves WHO is calling and records it on the request context for
// the handlers (identity.FromContext). It decides nothing and refuses nothing: enforcement is
// the route-class guard's job, which reads this identity.
//
//   - A request the capability middleware marked as the person's (a valid human capability for
//     exactly this request) is the human.
//   - Otherwise a valid session proof makes the request that session's, taken from the daemon's
//     own record of the session, never from the request.
//   - Anything else, including a stale or malformed proof, a proof of an ended session, and any
//     request that arrived on the LAN listener, is anonymous. (A phone has its own credential,
//     handled where LAN requests are classified.)
//
// A refused proof is logged at debug level with the reason only, never the proof.
func callerIdentityMiddleware(resolver *identity.Resolver, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			caller := identity.Anonymous()
			switch {
			case requestscope.IsLAN(r.Context()):
				// LAN requests are never identified by a local capability or proof.
			case requestscope.IsHumanUI(r.Context()):
				caller = identity.Human()
			case resolver != nil:
				proof := r.Header.Get(identity.HeaderProof)
				if proof == "" {
					// No proof: a session that was already running when proofs arrived may still be
					// recognised by the process tree it is in. Never for a caller that PRESENTED a
					// proof (a wrong one is simply refused), only for a loopback peer, and only from
					// the socket and the kernel (never from the request).
					if client, server, ok := loopbackPorts(r); ok && resolver.HasAncestry() {
						if resolved, err := resolver.ResolveByAncestry(r.Context(), client, server); err == nil {
							caller = resolved
						} else {
							log.Debug("process ancestry did not identify the caller", "reason", err.Error(), "method", r.Method, "path", r.URL.Path)
						}
					}
				} else {
					resolved, err := resolver.Resolve(r.Context(), proof)
					switch {
					case err == nil:
						caller = resolved
					case errors.Is(err, identity.ErrInvalid), errors.Is(err, identity.ErrSessionEnded), errors.Is(err, identity.ErrMissing):
						log.Debug("session proof refused", "reason", err.Error(), "method", r.Method, "path", r.URL.Path)
					default:
						log.Warn("session proof could not be checked", "error", err, "method", r.Method, "path", r.URL.Path)
					}
				}
			}
			next.ServeHTTP(w, r.WithContext(identity.WithCaller(r.Context(), caller)))
		})
	}
}

// loopbackPorts returns the client's and the daemon's ports of a loopback connection, taken from
// the connection itself (never from a header).
func loopbackPorts(r *http.Request) (client, server int, ok bool) {
	host, port, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return 0, 0, false
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return 0, 0, false
	}
	local, isTCP := r.Context().Value(http.LocalAddrContextKey).(*net.TCPAddr)
	if !isTCP || !local.IP.IsLoopback() {
		return 0, 0, false
	}
	c, err := strconv.Atoi(port)
	if err != nil || c <= 0 {
		return 0, 0, false
	}
	return c, local.Port, true
}
