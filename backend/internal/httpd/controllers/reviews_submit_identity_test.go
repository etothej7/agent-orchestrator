package controllers

import (
	"context"
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/identity"
	reviewsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/review"
)

type fakeRuns map[string]domain.ReviewRun

func (f fakeRuns) GetReviewRun(_ context.Context, id string) (domain.ReviewRun, bool, error) {
	r, ok := f[id]
	return r, ok, nil
}

type failingRuns struct{}

func (failingRuns) GetReviewRun(context.Context, string) (domain.ReviewRun, bool, error) {
	return domain.ReviewRun{}, false, errors.New("db down")
}

func run(id, review string, worker domain.SessionID, status domain.ReviewRunStatus) domain.ReviewRun {
	return domain.ReviewRun{ID: id, ReviewID: review, SessionID: worker, Status: status}
}

func submitted(ids ...string) []reviewsvc.SubmittedReview {
	var out []reviewsvc.SubmittedReview
	for _, id := range ids {
		out = append(out, reviewsvc.SubmittedReview{RunID: id})
	}
	return out
}

var (
	reviewerOf = func(review string, worker domain.SessionID) identity.Caller {
		return identity.Caller{Kind: identity.KindReviewer, Review: review, Worker: worker, Project: "repo"}
	}
	agentCaller = func(id domain.SessionID, kind domain.SessionKind) identity.Caller {
		return identity.Caller{Kind: identity.KindAgent, Session: id, SessionKind: kind, Project: "repo"}
	}
)

// Review submit is the reviewer's act, for ITS OWN active run of the worker in the path: not the
// worker under review, not its manager, not another reviewer, not a finished run.
func TestOnlyTheBoundReviewerOfAnActiveRunMaySubmit(t *testing.T) {
	runs := fakeRuns{
		"run-1":   run("run-1", "review-1", "worker-1", domain.ReviewRunRunning),
		"run-2":   run("run-2", "review-2", "worker-1", domain.ReviewRunRunning),
		"old":     run("old", "review-1", "worker-1", domain.ReviewRunComplete),
		"other-w": run("other-w", "review-1", "worker-9", domain.ReviewRunRunning),
		"orphan":  run("orphan", "review-1", "", domain.ReviewRunRunning),
	}
	cases := []struct {
		name    string
		caller  identity.Caller
		path    domain.SessionID
		reviews []reviewsvc.SubmittedReview
		reader  ReviewRunReader
		refused bool
	}{
		{"the person", identity.Human(), "worker-1", submitted("run-1"), runs, false},
		{"nobody (the guard decides, not the controller)", identity.Anonymous(), "worker-1", submitted("run-1"), runs, false},
		{"the bound reviewer, its active run", reviewerOf("review-1", "worker-1"), "worker-1", submitted("run-1"), runs, false},
		{"the worker under review", agentCaller("worker-1", domain.KindWorker), "worker-1", submitted("run-1"), runs, true},
		{"the project's manager", agentCaller("manager-1", domain.KindOrchestrator), "worker-1", submitted("run-1"), runs, true},
		{"a reviewer of another review", reviewerOf("review-2", "worker-1"), "worker-1", submitted("run-1"), runs, true},
		{"a finished run", reviewerOf("review-1", "worker-1"), "worker-1", submitted("old"), runs, true},
		{"a run of another worker", reviewerOf("review-1", "worker-1"), "worker-1", submitted("other-w"), runs, true},
		{"a worker in the path that is not its worker", reviewerOf("review-1", "worker-1"), "worker-2", submitted("run-1"), runs, true},
		{"a run with no worker recorded fails closed", reviewerOf("review-1", "worker-1"), "worker-1", submitted("orphan"), runs, true},
		{"an unknown run", reviewerOf("review-1", "worker-1"), "worker-1", submitted("nope"), runs, true},
		{"one bad run in a batch refuses the batch", reviewerOf("review-1", "worker-1"), "worker-1", submitted("run-1", "run-2"), runs, true},
		{"a reviewer with no run reader fails closed", reviewerOf("review-1", "worker-1"), "worker-1", submitted("run-1"), nil, true},
		{"a failing run lookup fails closed", reviewerOf("review-1", "worker-1"), "worker-1", submitted("run-1"), failingRuns{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := submitterRefusal(context.Background(), tc.path, tc.caller, tc.reviews, tc.reader)
			if (got != "") != tc.refused {
				t.Fatalf("refusal = %q, want refused=%v", got, tc.refused)
			}
		})
	}
}
