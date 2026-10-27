package controllers

import (
	"net/http"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/requestscope"
)

// Human-authored writes.
//
// Who wrote something is a security fact, not a label. The daemon treats a request as the
// person's only when it carries a per-request capability the desktop app's main process signed
// (package uicapability), which the capability middleware verified and recorded in the request
// context. Nothing a client can put in a body, header or environment makes a request human.
// A route only the person may call is wrapped in humanOnly: without a verified capability the
// answer is 403 HUMAN_CAPABILITY_REQUIRED.

const humanCapabilityRequiredCode = "HUMAN_CAPABILITY_REQUIRED"

const humanCapabilityRequiredMessage = "this action is only available to the person using the AO app; " +
	"an agent or the CLI cannot perform it. Ask the person to do it in the app, or file a request with ao report --needs-input"

// isHumanUI reports whether the daemon verified a human-UI capability for r.
func isHumanUI(r *http.Request) bool { return requestscope.IsHumanUI(r.Context()) }

// writeHumanCapabilityRequired answers a refused human-only write.
func writeHumanCapabilityRequired(w http.ResponseWriter, r *http.Request) {
	envelope.WriteAPIError(w, r, http.StatusForbidden, "forbidden", humanCapabilityRequiredCode, humanCapabilityRequiredMessage, nil)
}

// humanOnly guards a route that only the person at the keyboard may call.
func humanOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isHumanUI(r) {
			writeHumanCapabilityRequired(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}
