package routeclass

import (
	"slices"
	"testing"
)

// The observe flags decide who an anonymous (un-proven) session may still reach, so they are
// pinned here: a change is a deliberate edit of this list, reviewed against ed.7's behaviour.
func TestObserveFlagsArePinned(t *testing.T) {
	refuseAnonymous := []string{
		"POST /api/v1/prs/{id}/merge",
		"POST /api/v1/sessions/{sessionId}/reviews/submit",
		"POST /api/v1/sessions/{sessionId}/kill",
		"POST /api/v1/sessions/{sessionId}/exit-agent",
		"POST /api/v1/sessions/{sessionId}/rollback",
		"POST /api/v1/sessions/cleanup",
		"POST /api/v1/orchestrators",
		"POST /api/v1/orchestrators/delegate",
		// scheduling autonomous agent work
		"POST /api/v1/automations",
		"PATCH /api/v1/automations/{automationId}",
		"DELETE /api/v1/automations/{automationId}",
		"PATCH /api/v1/cues/{cueId}",
		"DELETE /api/v1/cues/{cueId}",
		"POST /api/v1/cues/{cueId}/invoke",
		"POST /api/v1/projects/{projectId}/cues",
	}
	coordinatorOnly := []string{
		"POST /api/v1/sessions/{sessionId}/conversation/human-decisions",
		"POST /api/v1/sessions/{sessionId}/conversation/human-decisions/resolve",
		"POST /api/v1/workspace/intake",
		"POST /api/v1/workspace/epics",
		"POST /api/v1/workspace/stories",
	}
	kinds := []string{"POST /api/v1/workspace/commands", "POST /api/v1/sessions"}
	var gotNone, gotCoord, gotKinds []string
	for _, e := range Entries() {
		key := e.Method + " " + e.Pattern
		if e.Class != A {
			if e.Observe != ObserveNone || len(e.Kinds) != 0 {
				t.Errorf("%s is class %s but carries an observe flag", key, e.Class)
			}
			continue
		}
		switch e.Observe {
		case ObserveNone:
			gotNone = append(gotNone, key)
		case ObserveUntilCoordinatorProof:
			gotCoord = append(gotCoord, key)
		case ObserveKinds:
			gotKinds = append(gotKinds, key)
			if len(e.Kinds) == 0 {
				t.Errorf("%s is ObserveKinds with no kinds", key)
			}
		case ObserveOK:
		default:
			t.Errorf("%s has an unknown observe flag %q", key, e.Observe)
		}
		if e.Observe != ObserveKinds && len(e.Kinds) != 0 {
			t.Errorf("%s lists kinds but is not ObserveKinds", key)
		}
	}
	for name, pair := range map[string][2][]string{"refuse-anonymous": {gotNone, refuseAnonymous}, "coordinator-only": {gotCoord, coordinatorOnly}, "kinds": {gotKinds, kinds}} {
		got, want := slices.Clone(pair[0]), slices.Clone(pair[1])
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("%s routes = %v, want %v", name, got, want)
		}
	}
}

// Authority kinds of a story command are never observe-allowed.
func TestStoryAuthorityKindsAreNotObserveAllowed(t *testing.T) {
	e, ok := Lookup("POST", "/api/v1/workspace/commands")
	if !ok {
		t.Fatal("workspace/commands is not classified")
	}
	for _, kind := range []string{"done", "assignment", "ready_for_approval", "answer", "answer_confirmation", ""} {
		if slices.Contains(e.Kinds, kind) {
			t.Errorf("story command kind %q is observe-allowed", kind)
		}
	}
	spawn, _ := Lookup("POST", "/api/v1/sessions")
	if slices.Contains(spawn.Kinds, "orchestrator") {
		t.Error("spawning an orchestrator is observe-allowed")
	}
}
