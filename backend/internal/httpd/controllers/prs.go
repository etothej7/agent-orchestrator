package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	"github.com/aoagents/agent-orchestrator/backend/internal/identity"
	humanrequestsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/humanrequest"
	prsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/pr"
)

var prIDPattern = regexp.MustCompile(`^[1-9]\d*$`)

// PRMergeStore is what the merge route reads to decide who may merge: the tracked pull request, the
// session that owns it, and that session's project (whose agentMayMerge setting applies).
type PRMergeStore interface {
	GetPR(ctx context.Context, url string) (domain.PullRequest, bool, error)
	GetSession(ctx context.Context, id domain.SessionID) (domain.SessionRecord, bool, error)
	GetProject(ctx context.Context, id string) (domain.ProjectRecord, bool, error)
}

// SessionReproof asks the daemon to give a live session that has no identity proof one at its next idle moment. It
// reports whether it did: false for a session that does not exist, has ended, or already has a proof.
type SessionReproof interface {
	Prioritize(ctx context.Context, id domain.SessionID) bool
}

// PRsController owns the /prs action routes.
//
// With Store set, a merge is decided by who is calling (identity.DecideMerge): the person always may; an agent
// that is the pull request's own worker or the project's manager may when the project allows agent merges, and
// otherwise files a pending request for the person; any other agent or a reviewer is refused. A caller that
// proved nothing but claims to be a live session is told to be relaunched rather than getting a request. Without
// Store the route is as before (the daemon always sets it; unit tests and headless builds may not).
type PRsController struct {
	Svc      prsvc.ActionManager
	Store    PRMergeStore
	Requests HumanRequestService
	// Reproof, with the caller's X-AO-Session-Claim (a claim, never an identity), tells an unproven live session
	// from a terminal. Nil treats every unproven caller as a terminal.
	Reproof SessionReproof
}

// Register mounts the PR action routes on the supplied router.
func (c *PRsController) Register(r chi.Router) {
	r.Post("/prs/{id}/merge", c.merge)
	r.Post("/prs/{id}/resolve-comments", c.resolveComments)
}

func (c *PRsController) merge(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "POST", "/api/v1/prs/{id}/merge")
		return
	}
	prID := chi.URLParam(r, "id")
	if !prIDPattern.MatchString(prID) {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_PR", "Invalid PR number", nil)
		return
	}
	var in MergePRRequest
	if err := decodeJSON(r, &in); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
		return
	}
	if strings.TrimSpace(in.PRURL) == "" || strings.TrimSpace(in.ExpectedHeadSHA) == "" {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_PR", "PR URL and expected head SHA are required", nil)
		return
	}
	if c.Store != nil && !c.mayMergeNow(w, r, prID, in) {
		return
	}
	res, err := c.Svc.Merge(r.Context(), prsvc.MergeRequest{PRID: prID, PRURL: in.PRURL, ExpectedHeadSHA: in.ExpectedHeadSHA})
	if err != nil {
		writePRError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, MergePRResponse{
		OK:             true,
		PRNumber:       res.PRNumber,
		Method:         res.Method,
		State:          res.State,
		StateChangedAt: res.StateChangedAt,
	})
}

// mayMergeNow applies the caller's authority to a merge. It returns true when the merge should go on; otherwise it
// has already answered (a refusal, or the filed request).
func (c *PRsController) mayMergeNow(w http.ResponseWriter, r *http.Request, prID string, in MergePRRequest) bool {
	ctx := r.Context()
	caller := identity.FromContext(ctx)
	if caller.IsHuman() {
		return true
	}
	pr, found, err := c.Store.GetPR(ctx, strings.TrimSpace(in.PRURL))
	if err != nil {
		envelope.WriteAPIError(w, r, http.StatusInternalServerError, "internal", "PR_OPERATION_FAILED", "PR operation failed", nil)
		return false
	}
	if !found || strconv.Itoa(pr.Number) != prID {
		// Nothing tracked to decide about, so nobody but the person may act on it (the service would say the same).
		envelope.WriteAPIError(w, r, http.StatusNotFound, "not_found", "PR_NOT_FOUND", "Unknown PR", nil)
		return false
	}
	owner, found, err := c.Store.GetSession(ctx, pr.SessionID)
	if err != nil {
		envelope.WriteAPIError(w, r, http.StatusInternalServerError, "internal", "PR_OPERATION_FAILED", "PR operation failed", nil)
		return false
	}
	if !found {
		envelope.WriteAPIError(w, r, http.StatusNotFound, "not_found", "PR_NOT_FOUND", "Unknown PR", nil)
		return false
	}
	project, _, err := c.Store.GetProject(ctx, string(owner.ProjectID))
	if err != nil {
		envelope.WriteAPIError(w, r, http.StatusInternalServerError, "internal", "PR_OPERATION_FAILED", "PR operation failed", nil)
		return false
	}
	switch identity.DecideMerge(caller, project.Config.AgentMergeAllowed(), identity.Target{Session: pr.SessionID, Project: owner.ProjectID}) {
	case identity.Allow:
		return true
	case identity.Refuse:
		envelope.WriteAPIError(w, r, http.StatusForbidden, "forbidden", "MERGE_NOT_PERMITTED", "Only the pull request's own worker or its project's manager may merge it, and only when the project allows agent merges. Ask the person.", nil)
		return false
	}
	// NeedsHuman. A caller that proved nothing but claims to be a live session is a session that started before
	// it could be given a proof: it is asked to be relaunched, and it is not given a request the person would
	// have to approve on its behalf for every routine action.
	if caller.Kind == identity.KindAnonymous && c.Reproof != nil {
		if claimed := domain.SessionID(strings.TrimSpace(r.Header.Get(identity.HeaderClaim))); claimed != "" && len(claimed) <= 128 && c.Reproof.Prioritize(ctx, claimed) {
			envelope.WriteAPIError(w, r, http.StatusForbidden, "forbidden", "SESSION_NEEDS_IDENTITY", "This session has no identity proof yet; it will be relaunched automatically when idle.", nil)
			return false
		}
	}
	if c.Requests == nil {
		envelope.WriteAPIError(w, r, http.StatusForbidden, "forbidden", "MERGE_NOT_PERMITTED", "Only the person may merge this pull request.", nil)
		return false
	}
	payload, err := json.Marshal(map[string]string{"prId": prID, "prUrl": strings.TrimSpace(in.PRURL), "expectedHeadSha": strings.TrimSpace(in.ExpectedHeadSHA)})
	if err != nil {
		envelope.WriteAPIError(w, r, http.StatusInternalServerError, "internal", "PR_OPERATION_FAILED", "PR operation failed", nil)
		return false
	}
	req, err := c.Requests.File(ctx, humanrequestsvc.FileInput{
		Kind: domain.HumanRequestPRMerge, ProjectID: string(owner.ProjectID), Payload: payload, Requester: requesterOf(caller),
	})
	if err != nil {
		envelope.WriteError(w, r, err)
		return false
	}
	envelope.WriteJSON(w, http.StatusAccepted, FileHumanRequestResponse{
		Request: humanRequestResponse(req),
		Message: "This project does not let agents merge, so the merge is not done. Filed request " + req.ID + " for the person to approve in the AO app; you will be told how it ends. Do not retry it.",
	})
	return false
}

func (c *PRsController) resolveComments(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "POST", "/api/v1/prs/{id}/resolve-comments")
		return
	}
	prID := chi.URLParam(r, "id")
	if !prIDPattern.MatchString(prID) {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_PR", "Invalid PR number", nil)
		return
	}

	// Body is optional: omitting it resolves all unresolved threads.
	var in ResolveCommentsRequest
	if err := decodeJSON(r, &in); err != nil && !isEmptyBody(err) {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
		return
	}

	res, err := c.Svc.ResolveComments(r.Context(), prID, in.CommentIDs)
	if err != nil {
		writePRError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, ResolveCommentsResponse{OK: true, Resolved: res.Resolved})
}

// writePRError maps PR sentinel errors to their locked HTTP envelopes,
// falling back to 500 for unexpected failures.
func writePRError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, prsvc.ErrInvalidPR):
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_PR", "Invalid PR", nil)
	case errors.Is(err, prsvc.ErrPRNotFound):
		envelope.WriteAPIError(w, r, http.StatusNotFound, "not_found", "PR_NOT_FOUND", "Unknown PR", nil)
	case errors.Is(err, prsvc.ErrPRNotMergeable):
		envelope.WriteAPIError(w, r, http.StatusConflict, "conflict", "PR_NOT_MERGEABLE", "PR is not mergeable", nil)
	case errors.Is(err, prsvc.ErrPRHeadChanged):
		envelope.WriteAPIError(w, r, http.StatusConflict, "conflict", "PR_HEAD_CHANGED", "PR head changed; refresh before merging", nil)
	case errors.Is(err, prsvc.ErrPRPreconditions):
		envelope.WriteAPIError(w, r, http.StatusUnprocessableEntity, "unprocessable", "PR_PRECONDITIONS_UNMET", "PR merge preconditions are not met", nil)
	case errors.Is(err, prsvc.ErrNothingToResolve):
		envelope.WriteAPIError(w, r, http.StatusUnprocessableEntity, "unprocessable", "NOTHING_TO_RESOLVE", "No unresolved review threads to resolve", nil)
	default:
		envelope.WriteAPIError(w, r, http.StatusInternalServerError, "internal", "PR_OPERATION_FAILED", "PR operation failed", nil)
	}
}

// isEmptyBody reports whether err signals an absent or empty request body.
// io.ErrUnexpectedEOF means a truncated/malformed body — bad request, not absent.
func isEmptyBody(err error) bool {
	return errors.Is(err, io.EOF)
}
