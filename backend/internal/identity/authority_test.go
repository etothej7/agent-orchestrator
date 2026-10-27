package identity

import (
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func agentCaller(id domain.SessionID, kind domain.SessionKind, project domain.ProjectID) Caller {
	return Caller{Kind: KindAgent, Session: id, SessionKind: kind, Project: project}
}

// The merge decision (design D1): a merge uses the person's GitHub identity, so it is the
// person's unless the project turned agent merging on, and then only for the pull request's own
// worker or the project's manager.
func TestDecideMergeBindsAgentsAndNeverRefusesThePerson(t *testing.T) {
	pr := Target{Session: "worker-1", Project: "repo"}
	cases := []struct {
		name   string
		caller Caller
		allow  bool
		want   Verdict
	}{
		{"the person, setting off", Human(), false, Allow},
		{"the person, setting on", Human(), true, Allow},
		{"the PR's own worker, setting on", agentCaller("worker-1", domain.KindWorker, "repo"), true, Allow},
		{"the project's manager, setting on", agentCaller("manager-1", domain.KindOrchestrator, "repo"), true, Allow},
		{"the PR's own worker, setting off", agentCaller("worker-1", domain.KindWorker, "repo"), false, NeedsHuman},
		{"the project's manager, setting off", agentCaller("manager-1", domain.KindOrchestrator, "repo"), false, NeedsHuman},
		{"another worker, setting on", agentCaller("worker-2", domain.KindWorker, "repo"), true, Refuse},
		{"a manager of another project, setting on", agentCaller("manager-9", domain.KindOrchestrator, "other"), true, Refuse},
		{"a reviewer, setting on", Caller{Kind: KindReviewer, Review: "review-1", Worker: "worker-1", Project: "repo"}, true, Refuse},
		{"nobody (a terminal), setting on", Anonymous(), true, NeedsHuman},
		{"nobody (a terminal), setting off", Anonymous(), false, NeedsHuman},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DecideMerge(tc.caller, tc.allow, pr); got != tc.want {
				t.Fatalf("DecideMerge = %v, want %v", got, tc.want)
			}
		})
	}
}

// Submitting a review is the reviewer's act, for its own review run only. Neither the worker
// being reviewed nor a manager may submit one (independence), and the person's own click is the
// UI's.
func TestMaySubmitReviewIsTheReviewersOfThatRunOrThePerson(t *testing.T) {
	run := ReviewRunRef{Review: "review-1", Worker: "worker-1"}
	reviewer := Caller{Kind: KindReviewer, Review: "review-1", Worker: "worker-1", Project: "repo"}
	for name, tc := range map[string]struct {
		caller Caller
		want   bool
	}{
		"the reviewer of that run":     {reviewer, true},
		"the person":                   {Human(), true},
		"the reviewer of another run":  {Caller{Kind: KindReviewer, Review: "review-2", Worker: "worker-2"}, false},
		"the worker being reviewed":    {agentCaller("worker-1", domain.KindWorker, "repo"), false},
		"the project's manager":        {agentCaller("manager-1", domain.KindOrchestrator, "repo"), false},
		"nobody":                       {Anonymous(), false},
		"a reviewer shell with no run": {Caller{Kind: KindReviewer}, false},
	} {
		if got := MaySubmitReview(tc.caller, run); got != tc.want {
			t.Errorf("%s: MaySubmitReview = %v, want %v", name, got, tc.want)
		}
	}
}

func TestSelfAndManagerPredicatesNeedAVerifiedAgent(t *testing.T) {
	worker := agentCaller("worker-1", domain.KindWorker, "repo")
	manager := agentCaller("manager-1", domain.KindOrchestrator, "repo")
	if !worker.IsSession("worker-1") || worker.IsSession("worker-2") || Anonymous().IsSession("worker-1") || Human().IsSession("worker-1") {
		t.Fatal("IsSession must be true only for the verified session itself")
	}
	if !manager.IsManagerOf("repo") || manager.IsManagerOf("other") || worker.IsManagerOf("repo") || Anonymous().IsManagerOf("repo") {
		t.Fatal("IsManagerOf must be true only for a verified orchestrator of that project")
	}
	// A reviewer carries no session, so it can never satisfy a session predicate even if its
	// fields were set to look like one.
	forged := Caller{Kind: KindReviewer, Review: "review-1", Session: "worker-1", SessionKind: domain.KindWorker}
	if forged.IsSession("worker-1") || forged.IsManagerOf("") {
		t.Fatal("a reviewer satisfied a session predicate")
	}
}
