package controllers

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/identity"
	reviewsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/review"
)

// ReviewRunReader reads a review run by id, for binding a reviewer to its own run.
type ReviewRunReader interface {
	GetReviewRun(ctx context.Context, id string) (domain.ReviewRun, bool, error)
}

// submitterRefusal decides whether a VERIFIED caller may submit these reviews for the worker in the
// path, returning a reason for a refusal ("" = allowed). Review submit is the native reviewer's
// act for its own run: the person may always do it; a verified agent (the worker under review, its
// manager, anyone else) may not (a review by its own subject is not independent); a verified
// reviewer may only submit an ACTIVE run of its own review, for the worker in the path, and the
// run must name that worker. Every lookup failure refuses. An anonymous caller is left to the
// route-class guard, which refuses it on this route in every mode.
func submitterRefusal(ctx context.Context, path domain.SessionID, caller identity.Caller, reviews []reviewsvc.SubmittedReview, runs ReviewRunReader) string {
	switch {
	case caller.IsHuman(), caller.Kind == identity.KindAnonymous:
		return ""
	case !caller.IsReviewer():
		return "Only the reviewer of this run may submit its review."
	case runs == nil:
		return "The reviewer's run cannot be verified."
	}
	for _, review := range reviews {
		run, found, err := runs.GetReviewRun(ctx, review.RunID)
		switch {
		case err != nil || !found:
			return "The reviewer's run cannot be verified."
		case run.Status != domain.ReviewRunRunning:
			return "That review run is no longer active."
		case run.SessionID != path:
			return "That review run is for another worker."
		case !identity.MaySubmitReview(caller, identity.ReviewRunRef{Review: run.ReviewID, Worker: run.SessionID}):
			return "That review run belongs to another reviewer."
		}
	}
	return ""
}
