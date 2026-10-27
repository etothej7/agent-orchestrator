package identity

import (
	"context"
	"strings"
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

// The run's worker is part of the binding: a run that does not say whose worker it is cannot be
// matched to a reviewer, so nobody may submit it (fail closed), not even its own reviewer.
func TestMaySubmitReviewFailsClosedWhenTheRunNamesNoWorker(t *testing.T) {
	reviewer := Caller{Kind: KindReviewer, Review: "review-1", Worker: "worker-1", Project: "repo"}
	if MaySubmitReview(reviewer, ReviewRunRef{Review: "review-1", Worker: ""}) {
		t.Fatal("a run with no worker recorded was submittable by its reviewer")
	}
	if MaySubmitReview(Caller{Kind: KindReviewer, Review: "review-1"}, ReviewRunRef{Review: "review-1", Worker: "worker-1"}) {
		t.Fatal("a reviewer with no worker recorded could submit")
	}
	if !MaySubmitReview(Human(), ReviewRunRef{}) {
		t.Fatal("the person was refused")
	}
}

// The overall coordinator coordinates; it does not merge. It is an orchestrator of the coordinator
// project, so it is neither the PR's own worker nor that PR's project manager, and DecideMerge
// refuses it for every repository PR EVEN WHEN the project turned agent merging on (Ed's no-merge
// rule for the coordinator).
func TestTheOverallCoordinatorIsNeverAllowedToMerge(t *testing.T) {
	coordinator := agentCaller("coord-1", domain.KindOrchestrator, "overall-coordinator")
	for _, allow := range []bool{false, true} {
		if got := DecideMerge(coordinator, allow, Target{Session: "worker-1", Project: "repo"}); got != Refuse {
			t.Errorf("agentMayMerge=%v: the overall coordinator's merge verdict = %v, want refuse", allow, got)
		}
	}
	// Control: the repository's own manager is allowed when the setting is on.
	if got := DecideMerge(agentCaller("manager-1", domain.KindOrchestrator, "repo"), true, Target{Session: "worker-1", Project: "repo"}); got != Allow {
		t.Errorf("the project's manager = %v, want allow", got)
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

// A caller the daemon matched only by process ancestry has no authority: every predicate and
// decision treats it as the anonymous caller.
func TestACallerKnownOnlyByAncestryFailsEveryPredicate(t *testing.T) {
	for _, kind := range []domain.SessionKind{domain.KindWorker, domain.KindOrchestrator} {
		c := Caller{Kind: KindAgent, Session: "s-1", SessionKind: kind, Project: "repo", Via: ViaAncestry}
		if c.IsAgent() || c.IsSession("s-1") || c.IsManagerOf("repo") || c.IsHuman() || c.IsReviewer() {
			t.Errorf("%s: an ancestry caller satisfied a predicate: agent=%v session=%v manager=%v", kind, c.IsAgent(), c.IsSession("s-1"), c.IsManagerOf("repo"))
		}
		if !c.IsBridged() {
			t.Errorf("%s: not reported as bridged", kind)
		}
		for _, mayMerge := range []bool{false, true} {
			if got := DecideMerge(c, mayMerge, Target{Session: "s-1", Project: "repo"}); got != NeedsHuman {
				t.Errorf("%s: DecideMerge(agentMayMerge=%v) = %v for its own PR; want NeedsHuman (as anonymous)", kind, mayMerge, got)
			}
		}
		if MaySubmitReview(c, ReviewRunRef{Review: "review-1", Worker: "s-1"}) {
			t.Errorf("%s: an ancestry caller may submit a review", kind)
		}
	}
	proven := Caller{Kind: KindAgent, Session: "s-1", SessionKind: domain.KindWorker, Project: "repo", Via: ViaProof}
	if !proven.IsAgent() || !proven.IsSession("s-1") || proven.IsBridged() {
		t.Fatal("a proven agent lost its predicates")
	}
}

func TestClaimFromContextAttributesOnlyUnprovenCallers(t *testing.T) {
	ctx := WithClaim(context.Background(), "worker-9")
	if got := ClaimFromContext(WithCaller(ctx, Anonymous())); got != "worker-9" {
		t.Fatalf("anonymous claim = %q, want worker-9", got)
	}
	if got := ClaimFromContext(context.Background()); got != "" {
		t.Fatalf("an unmarked context claims %q", got)
	}
	// a caller the kernel matched is attributed to the MATCHED session, not to what the header says
	bridged := Caller{Kind: KindAgent, Session: "worker-1", SessionKind: domain.KindWorker, Via: ViaAncestry}
	if got := ClaimFromContext(WithCaller(ctx, bridged)); got != "worker-1" {
		t.Fatalf("bridged claim = %q, want the matched worker-1", got)
	}
	// proven callers and the person are never "claimants"
	for name, c := range map[string]Caller{
		"human": Human(), "proven agent": {Kind: KindAgent, Session: "w", Via: ViaProof},
		"reviewer": {Kind: KindReviewer, Review: "r", Worker: "w"},
	} {
		if got := ClaimFromContext(WithCaller(ctx, c)); got != "" {
			t.Errorf("%s: claim %q, want none", name, got)
		}
	}
	for raw, want := range map[string]domain.SessionID{"  mer-1 ": "mer-1", "": "", "   ": "", strings.Repeat("x", 129): ""} {
		if got := ParseClaim(raw); got != want {
			t.Errorf("ParseClaim(%q) = %q, want %q", raw, got, want)
		}
	}
}
