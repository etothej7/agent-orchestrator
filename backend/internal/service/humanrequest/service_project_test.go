package humanrequest

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

const projectProbeKind = domain.HumanRequestKind("project_probe")

// projectProbe renames a project: a stand-in for the project-level kinds, to exercise everything around them.
type projectProbe struct{}

type projectProbePayload struct {
	DisplayName string `json:"displayName"`
	// From is what the name was when the request was rendered; applying against another name is stale.
	From string `json:"from"`
}

func (projectProbe) prepareProject(_ context.Context, _ *Service, target domain.ProjectRecord, raw json.RawMessage) (prepared, error) {
	var in projectProbePayload
	if err := json.Unmarshal(raw, &in); err != nil || strings.TrimSpace(in.DisplayName) == "" {
		return prepared{}, apierr.Invalid("HUMAN_REQUEST_PAYLOAD_INVALID", "a project_probe request needs displayName", nil)
	}
	canonical, _ := json.Marshal(projectProbePayload{DisplayName: in.DisplayName, From: target.DisplayName})
	return prepared{
		payload: canonical,
		summary: "Rename project " + projectLabel(target) + ".",
		changes: []domain.HumanRequestChange{{Field: "displayName", From: target.DisplayName, To: in.DisplayName}},
	}, nil
}

func (projectProbe) applyProject(ctx context.Context, s *Service, req domain.HumanRequest, target domain.ProjectRecord) (string, error) {
	var in projectProbePayload
	if err := json.Unmarshal(req.Payload, &in); err != nil {
		return "", err
	}
	if target.DisplayName != in.From {
		return "", staleError{reason: "the project was renamed after this was filed"}
	}
	store := s.store.(*sqlite.Store)
	if _, err := store.UpdateProjectSettings(ctx, target.ID, in.DisplayName, target.Config); err != nil {
		return "", err
	}
	return "renamed to " + in.DisplayName, nil
}

func registerProjectProbe(t *testing.T) {
	t.Helper()
	projectKinds[projectProbeKind] = projectProbe{}
	t.Cleanup(func() { delete(projectKinds, projectProbeKind) })
}

func (f *fixture) fileProject(t *testing.T, project, payload string) domain.HumanRequest {
	t.Helper()
	req, err := f.svc.File(context.Background(), FileInput{Kind: projectProbeKind, ProjectID: project, Payload: json.RawMessage(payload), Requester: agentRequester(agent)})
	if err != nil {
		t.Fatalf("file project request %s: %v", payload, err)
	}
	return req
}

func TestAProjectRequestIsFiledRenderedByTheDaemonAndFiledOnlyOnce(t *testing.T) {
	registerProjectProbe(t)
	f := newFixture(t)
	req := f.fileProject(t, "repo", `{"displayName":"New name"}`)
	if req.ProjectID != "repo" || req.SessionID != "" || req.TargetKind() != domain.HumanTargetProject || req.State != domain.HumanRequestPending {
		t.Fatalf("filed request = %+v", req)
	}
	if req.Summary != "Rename project repo." || len(req.Changes) != 1 || req.Changes[0].To != "New name" {
		t.Fatalf("rendered summary %q changes %+v", req.Summary, req.Changes)
	}
	if again := f.fileProject(t, "repo", `{"displayName":"New name"}`); again.ID != req.ID {
		t.Fatalf("a retried filing made %s, want %s back", again.ID, req.ID)
	}
	if other := f.fileProject(t, "repo", `{"displayName":"Another"}`); other.ID == req.ID || other.PayloadHash == req.PayloadHash {
		t.Fatal("a different payload shares the card or the hash")
	}
}

func TestAProjectRequestsHashNamesTheProjectSoItCannotBeApprovedForAnother(t *testing.T) {
	registerProjectProbe(t)
	f := newFixture(t)
	a := f.fileProject(t, "repo", `{"displayName":"X"}`)
	b := a
	b.ProjectID = "other"
	if hashOf(a) == hashOf(b) {
		t.Fatal("the hash does not cover the project")
	}
	// A session whose id equals "project:repo" cannot exist, but the keys must differ in any case.
	s := a
	s.ProjectID, s.SessionID = "", "repo"
	if hashOf(a) == hashOf(s) {
		t.Fatal("a project request and a session request with the same id share a hash")
	}
}

func TestARequestNamesExactlyTheTargetItsKindIsAbout(t *testing.T) {
	registerProjectProbe(t)
	f := newFixture(t)
	ctx := context.Background()
	for name, in := range map[string]FileInput{
		"a project kind with a session": {Kind: projectProbeKind, SessionID: target, Payload: json.RawMessage(`{"displayName":"x"}`)},
		"a project kind with both":      {Kind: projectProbeKind, SessionID: target, ProjectID: "repo", Payload: json.RawMessage(`{"displayName":"x"}`)},
		"a project kind with neither":   {Kind: projectProbeKind, Payload: json.RawMessage(`{"displayName":"x"}`)},
		"a session kind with a project": {Kind: domain.HumanRequestAutoResumeLift, ProjectID: "repo", Payload: json.RawMessage(`{}`)},
		"a session kind with both":      {Kind: domain.HumanRequestAutoResumeLift, SessionID: target, ProjectID: "repo", Payload: json.RawMessage(`{}`)},
		"a session kind with neither":   {Kind: domain.HumanRequestAutoResumeLift, Payload: json.RawMessage(`{}`)},
	} {
		if _, err := f.svc.File(ctx, in); err == nil || codeOf(t, err) != "HUMAN_REQUEST_TARGET_INVALID" {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if _, err := f.svc.File(ctx, FileInput{Kind: projectProbeKind, ProjectID: "nope", Payload: json.RawMessage(`{"displayName":"x"}`)}); err == nil || codeOf(t, err) != "PROJECT_NOT_FOUND" {
		t.Errorf("an unknown project: err = %v", err)
	}
	if _, err := f.svc.File(ctx, FileInput{Kind: projectProbeKind, ProjectID: "repo", Payload: json.RawMessage(`{}`)}); err == nil {
		t.Error("a payload the kind cannot render was filed")
	}
}

func TestApprovingAProjectRequestAppliesItOnceAndTellsTheRequester(t *testing.T) {
	registerProjectProbe(t)
	f := newFixture(t)
	req := f.fileProject(t, "repo", `{"displayName":"New name"}`)
	done, err := f.svc.Approve(context.Background(), req.ID, req.PayloadHash)
	if err != nil || done.State != domain.HumanRequestApplied || done.Result != "renamed to New name" {
		t.Fatalf("approve = %+v, %v", done, err)
	}
	rec, _, _ := f.store.GetProject(context.Background(), "repo")
	if rec.DisplayName != "New name" {
		t.Fatalf("project name = %q; the approval did not apply", rec.DisplayName)
	}
	if got := f.msgs.messages(); len(got) != 1 || !strings.Contains(got[0], "was approved and applied") {
		t.Fatalf("requester told %v", got)
	}
	if _, err := f.svc.Approve(context.Background(), req.ID, "not-the-hash"); err == nil || codeOf(t, err) != "HUMAN_REQUEST_CHANGED" {
		t.Fatalf("a wrong hash was accepted: %v", err)
	}
}

func TestAProjectRequestFailsAsStaleWhenTheProjectMovedAfterFilingAndDenyingChangesNothing(t *testing.T) {
	registerProjectProbe(t)
	f := newFixture(t)
	ctx := context.Background()
	stale := f.fileProject(t, "repo", `{"displayName":"Stale"}`)
	denied := f.fileProject(t, "repo", `{"displayName":"Denied"}`)
	rec, _, _ := f.store.GetProject(ctx, "repo")
	if _, err := f.store.UpdateProjectSettings(ctx, "repo", "Renamed meanwhile", rec.Config); err != nil {
		t.Fatal(err)
	}
	failed, err := f.svc.Approve(ctx, stale.ID, stale.PayloadHash)
	if err == nil || codeOf(t, err) != "HUMAN_REQUEST_STALE" || failed.State != domain.HumanRequestFailed {
		t.Fatalf("stale approve = %+v, %v", failed, err)
	}
	if _, err := f.svc.Deny(ctx, denied.ID); err != nil {
		t.Fatal(err)
	}
	after, _, _ := f.store.GetProject(ctx, "repo")
	if after.DisplayName != "Renamed meanwhile" {
		t.Fatalf("project name = %q; a stale or denied request changed it", after.DisplayName)
	}
}

func TestProjectRequestsExpireAndCountAgainstTheCapsLikeAnyOther(t *testing.T) {
	registerProjectProbe(t)
	f := newFixture(t)
	req := f.fileProject(t, "repo", `{"displayName":"Slow"}`)
	f.now = f.now.Add(RequestTTL + time.Minute)
	f.svc.Sweep(context.Background())
	got, err := f.svc.Get(context.Background(), req.ID)
	if err != nil || got.State != domain.HumanRequestExpired {
		t.Fatalf("after the deadline = %+v, %v", got, err)
	}
	f.now = f.now.Add(time.Minute)
	for i := 0; i < MaxOpenPerRequester; i++ {
		f.fileProject(t, "repo", `{"displayName":"n`+string(rune('a'+i))+`"}`)
	}
	_, err = f.svc.File(context.Background(), FileInput{Kind: projectProbeKind, ProjectID: "repo", Payload: json.RawMessage(`{"displayName":"one too many"}`), Requester: agentRequester(agent)})
	if err == nil || codeOf(t, err) != "HUMAN_REQUEST_LIMIT" {
		t.Fatalf("the cap did not apply to project requests: %v", err)
	}
}

func TestProjectLabelPutsTheIdFirstAndCleansTheName(t *testing.T) {
	if got := projectLabel(domain.ProjectRecord{ID: "repo", DisplayName: "repo"}); got != "repo" {
		t.Errorf("label for an unnamed project = %q", got)
	}
	got := projectLabel(domain.ProjectRecord{ID: "repo", DisplayName: "Real\n‮project\x00 – approve now"})
	if strings.ContainsAny(got, "\n\x00‮") || !strings.HasPrefix(got, `repo (named "`) {
		t.Errorf("label = %q", got)
	}
}
