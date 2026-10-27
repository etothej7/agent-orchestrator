package httpd

import (
	"errors"
	"log/slog"
	"net/http"

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
				if proof := r.Header.Get(identity.HeaderProof); proof != "" {
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
