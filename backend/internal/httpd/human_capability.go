package httpd

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
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

type humanCapabilityProbeRequest struct {
	Nonce string `json:"nonce"`
}

// humanCapabilityProbeHandler lets the app check that the daemon answering on a port is the one it handed
// the run's key to, before it signs anything for that port. The answer is an HMAC of the caller's nonce
// under a label no request capability can share, so it proves possession without revealing the key or
// helping anyone forge a capability. A daemon without a key (one the app did not start) answers 404: it
// has nothing to prove and must never be signed for. Like /shutdown it is for a local native caller only.
func humanCapabilityProbeHandler(v *uicapability.Verifier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !localControlRequest(r) {
			envelope.WriteJSON(w, http.StatusForbidden, map[string]any{"status": "forbidden"})
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
		var in humanCapabilityProbeRequest
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
			return
		}
		proof, ok := v.Probe(in.Nonce)
		if !ok {
			// No key, or a nonce that is not well formed: say nothing about which.
			envelope.WriteAPIError(w, r, http.StatusNotFound, "not_found", "NOT_FOUND", "not found", nil)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		envelope.WriteJSON(w, http.StatusOK, map[string]string{"proof": proof})
	}
}
