package humanrequest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	prsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/pr"
)

// ---- pr_merge ----

// prMerge is a request to merge one pull request at one head commit. It is filed by the merge route when an
// agent (or an unidentified terminal) asks and the project has not let agents merge. The person's approval runs
// the same guarded merge the app's Merge button runs, so the provider's own checks (a passing native review on
// exactly this head, required checks green, the head unchanged) are repeated at the moment of approval.
type prMerge struct{}

// prMergePayload is the whole vocabulary of this kind, and all of it is structured: nothing in it is text for the
// person to read. The requester is never part of it; it comes from the verified caller.
type prMergePayload struct {
	PRID            string `json:"prId"`
	PRURL           string `json:"prUrl"`
	ExpectedHeadSHA string `json:"expectedHeadSha"`
}

var (
	prNumberPattern = regexp.MustCompile(`^[1-9]\d{0,8}$`)
	headSHAPattern  = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// maxTitleRunes bounds how much of a pull request's title the card shows.
const maxTitleRunes = 60

func (prMerge) prepareProject(ctx context.Context, s *Service, target domain.ProjectRecord, raw json.RawMessage) (prepared, error) {
	if s.prMerger() == nil {
		return prepared{}, apierr.NotImplemented("HUMAN_REQUEST_KIND_UNAVAILABLE", "merging pull requests is not available on this daemon")
	}
	var in prMergePayload
	if err := decodeStrict(raw, &in); err != nil {
		return prepared{}, err
	}
	in.ExpectedHeadSHA = strings.ToLower(strings.TrimSpace(in.ExpectedHeadSHA))
	in.PRURL = strings.TrimSpace(in.PRURL)
	if !prNumberPattern.MatchString(in.PRID) || in.PRURL == "" || !headSHAPattern.MatchString(in.ExpectedHeadSHA) {
		return prepared{}, apierr.Invalid("HUMAN_REQUEST_PAYLOAD_INVALID", "a pr_merge request needs prId (a pull request number), prUrl and the full 40-character expectedHeadSha", nil)
	}
	pr, found, err := s.store.GetPR(ctx, in.PRURL)
	if err != nil {
		return prepared{}, err
	}
	if !found || strconv.Itoa(pr.Number) != in.PRID {
		return prepared{}, apierr.NotFound("PR_NOT_FOUND", "no such tracked pull request")
	}
	// What the person is shown is the daemon's record of this PR on this project, not the requester's description.
	owner, found, err := s.store.GetSession(ctx, pr.SessionID)
	if err != nil {
		return prepared{}, err
	}
	if !found || string(owner.ProjectID) != target.ID {
		return prepared{}, apierr.Invalid("HUMAN_REQUEST_PAYLOAD_INVALID", "that pull request does not belong to this project", nil)
	}
	if pr.Merged || pr.Closed || pr.Draft {
		return prepared{}, apierr.Conflict("HUMAN_REQUEST_NOT_NEEDED", "that pull request is not open for merging", nil)
	}
	if !strings.EqualFold(pr.HeadSHA, in.ExpectedHeadSHA) {
		return prepared{}, apierr.Conflict("PR_HEAD_CHANGED", "the pull request head is not the commit this request names; read it again", nil)
	}
	payload, err := canonical(in)
	if err != nil {
		return prepared{}, err
	}
	title := cleanLabel(pr.Title, maxTitleRunes)
	what := fmt.Sprintf("pull request #%d", pr.Number)
	if title != "" {
		what = fmt.Sprintf("pull request #%d (titled %q)", pr.Number, title)
	}
	return prepared{
		payload: payload,
		summary: fmt.Sprintf("Merge %s of project %s at commit %s. AO squash-merges it only if the review and checks still pass and the commit has not changed.", what, projectLabel(target), in.ExpectedHeadSHA[:12]),
		changes: []domain.HumanRequestChange{
			{Field: "pullRequest", From: "open", To: "merged"},
			{Field: "headCommit", From: "", To: in.ExpectedHeadSHA[:12]},
		},
	}, nil
}

func (prMerge) applyProject(ctx context.Context, s *Service, req domain.HumanRequest, _ domain.ProjectRecord) (string, error) {
	merger := s.prMerger()
	if merger == nil {
		return "", errors.New("merging pull requests is not available on this daemon")
	}
	var in prMergePayload
	if err := json.Unmarshal(req.Payload, &in); err != nil {
		return "", err
	}
	res, err := merger.Merge(ctx, prsvc.MergeRequest{PRID: in.PRID, PRURL: in.PRURL, ExpectedHeadSHA: in.ExpectedHeadSHA})
	switch {
	case err == nil:
		return fmt.Sprintf("pull request #%d merged (%s)", res.PRNumber, res.Method), nil
	case errors.Is(err, prsvc.ErrPRHeadChanged):
		return "", staleError{reason: "the pull request changed after this was filed; ask again for the new head"}
	case errors.Is(err, prsvc.ErrPRPreconditions):
		return "", errors.New("the merge preconditions no longer hold (review passing on this head, required checks green)")
	case errors.Is(err, prsvc.ErrPRNotMergeable):
		return "", errors.New("the pull request cannot be merged now")
	case errors.Is(err, prsvc.ErrPRNotFound):
		return "", errors.New("the pull request is no longer tracked")
	default:
		return "", err
	}
}
