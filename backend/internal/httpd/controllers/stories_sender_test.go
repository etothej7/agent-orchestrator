package controllers

import (
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/identity"
)

// An agent cannot become the person by leaving senderSessionId out, or send as another session; a
// reviewer sends no story commands; the person and an unproven caller are left to the guard and
// the service as before.
func TestStoryCommandsMustBeSentAsTheCallerItself(t *testing.T) {
	worker := identity.Caller{Kind: identity.KindAgent, Session: "worker-1", SessionKind: domain.KindWorker, Via: identity.ViaProof}
	bridged := identity.Caller{Kind: identity.KindAgent, Session: "worker-1", SessionKind: domain.KindWorker, Via: identity.ViaAncestry}
	reviewer := identity.Caller{Kind: identity.KindReviewer, Review: "review-1", Worker: "worker-1"}
	for name, tc := range map[string]struct {
		caller identity.Caller
		sender domain.SessionID
		refuse bool
	}{
		"proven agent as itself":                 {worker, "worker-1", false},
		"proven agent with the sender left out":  {worker, "", true},
		"proven agent as another session":        {worker, "manager-1", true},
		"bridged agent as itself":                {bridged, "worker-1", false},
		"bridged agent with the sender left out": {bridged, "", true},
		"bridged agent as another session":       {bridged, "manager-1", true},
		"reviewer":                               {reviewer, "worker-1", true},
		"the person (the board)":                 {identity.Human(), "", false},
		"the person naming a session":            {identity.Human(), "manager-1", false},
		"anonymous naming a session (ed.7)":      {identity.Anonymous(), "worker-1", false},
	} {
		if got := storySenderRefusal(tc.caller, tc.sender) != ""; got != tc.refuse {
			t.Errorf("%s: refused = %v, want %v", name, got, tc.refuse)
		}
	}
}
