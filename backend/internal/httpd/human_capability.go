package httpd

import (
	"net/http"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/requestscope"
	"github.com/aoagents/agent-orchestrator/backend/internal/uicapability"
)

// humanCapabilityMiddleware marks a request as made by the person at the keyboard when
// it carries a valid per-request UI capability (see package uicapability). It never
// refuses anything: a missing, stale or invalid capability simply leaves the request
// unmarked, and each human-only write then decides for itself whether to refuse it or
// to record it as an agent's. With no verifier (a daemon the app did not start, or a
// test) no request is ever marked.
func humanCapabilityMiddleware(v *uicapability.Verifier, now func() time.Time) func(http.Handler) http.Handler {
	if now == nil {
		now = time.Now
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if v != nil {
				if header := r.Header.Get(uicapability.Header); header != "" {
					if err := v.VerifyRequest(header, r.Method, r.URL.EscapedPath(), now()); err == nil {
						r = r.WithContext(requestscope.WithHumanUI(r.Context()))
					}
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}
