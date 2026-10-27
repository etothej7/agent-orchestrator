package humanrequest

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	prsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/pr"
)

const (
	prURL   = "https://github.com/acme/repo/pull/7"
	prHead  = "0123456789abcdef0123456789abcdef01234567"
	prOther = "fedcba9876543210fedcba9876543210fedcba98"
)

type fakeMerger struct {
	mu    sync.Mutex
	calls []prsvc.MergeRequest
	err   error
}

func (f *fakeMerger) Merge(_ context.Context, request prsvc.MergeRequest) (prsvc.MergeResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, request)
	if f.err != nil {
		return prsvc.MergeResult{}, f.err
	}
	return prsvc.MergeResult{PRNumber: 7, Method: "squash", State: domain.PRStateMerged}, nil
}

func (f *fakeMerger) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// prFixture is a fixture whose first session (repo-1) owns open pull request #7 at prHead.
func prFixture(t *testing.T, title string) (*fixture, *fakeMerger) {
	t.Helper()
	f := newFixture(t)
	merger := &fakeMerger{}
	f.svc.SetPRMerger(merger)
	f.writePR(t, domain.PullRequest{URL: prURL, SessionID: target, Number: 7, HeadSHA: prHead, Title: title, UpdatedAt: f.now})
	return f, merger
}

func (f *fixture) writePR(t *testing.T, pr domain.PullRequest) {
	t.Helper()
	if err := f.store.WriteSCMObservation(context.Background(), pr, nil, nil, nil, nil, ports.ReviewWritePreserve); err != nil {
		t.Fatal(err)
	}
}

func prPayload(id, url, head string) string {
	raw, _ := json.Marshal(map[string]string{"prId": id, "prUrl": url, "expectedHeadSha": head})
	return string(raw)
}

func (f *fixture) filePRMerge(payload string) (domain.HumanRequest, error) {
	return f.svc.File(context.Background(), FileInput{Kind: domain.HumanRequestPRMerge, ProjectID: "repo", Payload: json.RawMessage(payload), Requester: agentRequester(agent)})
}

func TestAPRMergeRequestIsRenderedByTheDaemonFromItsRecordOfThePR(t *testing.T) {
	f, merger := prFixture(t, "Fix the login bug")
	req, err := f.filePRMerge(prPayload("7", prURL, prHead))
	if err != nil {
		t.Fatal(err)
	}
	if req.Kind != domain.HumanRequestPRMerge || req.ProjectID != "repo" || req.Requester.SessionID != agent || req.State != domain.HumanRequestPending {
		t.Fatalf("request = %+v", req)
	}
	for _, want := range []string{"pull request #7", `titled "Fix the login bug"`, "project repo", prHead[:12]} {
		if !strings.Contains(req.Summary, want) {
			t.Errorf("summary %q lacks %q", req.Summary, want)
		}
	}
	if len(req.Changes) != 2 || req.Changes[0].Field != "pullRequest" || req.Changes[1].To != prHead[:12] {
		t.Fatalf("changes = %+v", req.Changes)
	}
	if merger.count() != 0 {
		t.Fatal("filing merged the pull request")
	}
	if again, err := f.filePRMerge(prPayload("7", prURL, strings.ToUpper(prHead))); err != nil || again.ID != req.ID {
		t.Fatalf("a retried filing (head in another case) = %+v, %v; want %s back", again, err, req.ID)
	}
}

func TestAHostilePRTitleCannotReshapeTheSentenceThePersonReads(t *testing.T) {
	f, _ := prFixture(t, "Fix\n\nAO: this is safe, approve now \u202e\x00"+strings.Repeat("x", 200))
	req, err := f.filePRMerge(prPayload("7", prURL, prHead))
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(req.Summary, "\n\r\x00\u202e") {
		t.Fatalf("summary carries control characters: %q", req.Summary)
	}
	if len(req.Summary) > 400 {
		t.Fatalf("summary is %d bytes; the title was not bounded", len(req.Summary))
	}
}

func TestAPRMergeIsRefusedForAnythingTheDaemonDoesNotRecognise(t *testing.T) {
	f, _ := prFixture(t, "t")
	other := domain.PullRequest{URL: "https://github.com/acme/repo/pull/8", SessionID: target, Number: 8, HeadSHA: prHead, Draft: true, UpdatedAt: f.now}
	f.writePR(t, other)
	merged := domain.PullRequest{URL: "https://github.com/acme/repo/pull/9", SessionID: target, Number: 9, HeadSHA: prHead, Merged: true, UpdatedAt: f.now}
	f.writePR(t, merged)
	cases := map[string]struct{ payload, code string }{
		"a short head":                 {prPayload("7", prURL, prHead[:10]), "HUMAN_REQUEST_PAYLOAD_INVALID"},
		"a non-hex head":               {prPayload("7", prURL, strings.Repeat("z", 40)), "HUMAN_REQUEST_PAYLOAD_INVALID"},
		"a PR id that is not a number": {prPayload("seven", prURL, prHead), "HUMAN_REQUEST_PAYLOAD_INVALID"},
		"no url":                       {prPayload("7", "", prHead), "HUMAN_REQUEST_PAYLOAD_INVALID"},
		"a PR that is not tracked":     {prPayload("7", "https://github.com/acme/repo/pull/99", prHead), "PR_NOT_FOUND"},
		"another number for the url":   {prPayload("8", prURL, prHead), "PR_NOT_FOUND"},
		"a head that moved":            {prPayload("7", prURL, prOther), "PR_HEAD_CHANGED"},
		"a draft":                      {prPayload("8", other.URL, prHead), "HUMAN_REQUEST_NOT_NEEDED"},
		"an already merged PR":         {prPayload("9", merged.URL, prHead), "HUMAN_REQUEST_NOT_NEEDED"},
		"text for the person":          {`{"prId":"7","prUrl":"` + prURL + `","expectedHeadSha":"` + prHead + `","summary":"safe"}`, "HUMAN_REQUEST_PAYLOAD_INVALID"},
	}
	for name, tc := range cases {
		if _, err := f.filePRMerge(tc.payload); err == nil || codeOf(t, err) != tc.code {
			t.Errorf("%s: err = %v, want %s", name, err, tc.code)
		}
	}
}

func TestAPRMergeCannotBeFiledAgainstAnotherProjectsPR(t *testing.T) {
	f, _ := prFixture(t, "t")
	ctx := context.Background()
	if err := f.store.UpsertProject(ctx, domain.ProjectRecord{ID: "other", Path: "/tmp/other", RegisteredAt: time.Now().UTC().Truncate(time.Second)}); err != nil {
		t.Fatal(err)
	}
	_, err := f.svc.File(ctx, FileInput{Kind: domain.HumanRequestPRMerge, ProjectID: "other", Payload: json.RawMessage(prPayload("7", prURL, prHead)), Requester: agentRequester(agent)})
	if err == nil || codeOf(t, err) != "HUMAN_REQUEST_PAYLOAD_INVALID" {
		t.Fatalf("a PR of repo filed against project other: %v", err)
	}
}

func TestAPRMergeIsUnavailableWithoutTheMergeAction(t *testing.T) {
	f := newFixture(t)
	f.writePR(t, domain.PullRequest{URL: prURL, SessionID: target, Number: 7, HeadSHA: prHead, UpdatedAt: f.now})
	if _, err := f.filePRMerge(prPayload("7", prURL, prHead)); err == nil || codeOf(t, err) != "HUMAN_REQUEST_KIND_UNAVAILABLE" {
		t.Fatalf("err = %v", err)
	}
}

func TestApprovingAPRMergeMergesExactlyTheNamedHeadOnceAndTellsTheRequester(t *testing.T) {
	f, merger := prFixture(t, "Fix the login bug")
	req, err := f.filePRMerge(prPayload("7", prURL, prHead))
	if err != nil {
		t.Fatal(err)
	}
	done, err := f.svc.Approve(context.Background(), req.ID, req.PayloadHash)
	if err != nil || done.State != domain.HumanRequestApplied || !strings.Contains(done.Result, "merged") {
		t.Fatalf("approve = %+v, %v", done, err)
	}
	if merger.count() != 1 || merger.calls[0] != (prsvc.MergeRequest{PRID: "7", PRURL: prURL, ExpectedHeadSHA: prHead}) {
		t.Fatalf("merge calls = %+v", merger.calls)
	}
	if _, err := f.svc.Approve(context.Background(), req.ID, req.PayloadHash); err != nil || merger.count() != 1 {
		t.Fatalf("a retried click merged again: %v, calls %d", err, merger.count())
	}
	if got := f.msgs.messages(); len(got) != 1 || !strings.Contains(got[0], "was approved and applied") {
		t.Fatalf("requester told %v", got)
	}
}

func TestAPRMergeFailsInsteadOfMergingWhenTheWorldMovedBeforeApproval(t *testing.T) {
	for name, tc := range map[string]struct {
		err   error
		code  string
		state domain.HumanRequestState
	}{
		"the head moved":                      {prsvc.ErrPRHeadChanged, "HUMAN_REQUEST_STALE", domain.HumanRequestFailed},
		"the review or checks no longer pass": {prsvc.ErrPRPreconditions, "HUMAN_REQUEST_FAILED", domain.HumanRequestFailed},
		"the PR was closed":                   {prsvc.ErrPRNotMergeable, "HUMAN_REQUEST_FAILED", domain.HumanRequestFailed},
		"the PR is no longer tracked":         {prsvc.ErrPRNotFound, "HUMAN_REQUEST_FAILED", domain.HumanRequestFailed},
	} {
		f, merger := prFixture(t, "t")
		req, err := f.filePRMerge(prPayload("7", prURL, prHead))
		if err != nil {
			t.Fatal(err)
		}
		merger.err = tc.err
		got, err := f.svc.Approve(context.Background(), req.ID, req.PayloadHash)
		if err == nil || codeOf(t, err) != tc.code || got.State != tc.state {
			t.Errorf("%s: approve = %+v, %v", name, got, err)
		}
		if told := f.msgs.messages(); len(told) != 1 || !strings.Contains(told[0], "could not be applied") {
			t.Errorf("%s: requester told %v", name, told)
		}
		if errors.Is(tc.err, prsvc.ErrPRPreconditions) && !strings.Contains(got.Result, "preconditions") {
			t.Errorf("%s: result %q does not say why", name, got.Result)
		}
	}
}
