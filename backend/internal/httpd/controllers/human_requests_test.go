package controllers_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	humanrequestsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/humanrequest"
	"github.com/aoagents/agent-orchestrator/backend/internal/uicapability"
)

type fakeHumanRequests struct {
	filed       []humanrequestsvc.FileInput
	listedAll   []bool
	approved    []string
	denied      []string
	approveErr  error
	request     domain.HumanRequest
	notFoundGet bool
}

func (f *fakeHumanRequests) File(_ context.Context, in humanrequestsvc.FileInput) (domain.HumanRequest, error) {
	f.filed = append(f.filed, in)
	return f.request, nil
}

func (f *fakeHumanRequests) List(_ context.Context, all bool) ([]domain.HumanRequest, error) {
	f.listedAll = append(f.listedAll, all)
	return []domain.HumanRequest{f.request}, nil
}

func (f *fakeHumanRequests) Get(_ context.Context, id string) (domain.HumanRequest, error) {
	if f.notFoundGet {
		return domain.HumanRequest{}, apierr.NotFound("HUMAN_REQUEST_NOT_FOUND", "no such request")
	}
	return f.request, nil
}

func (f *fakeHumanRequests) Approve(_ context.Context, id, hash string) (domain.HumanRequest, error) {
	f.approved = append(f.approved, id+":"+hash)
	return f.request, f.approveErr
}

func (f *fakeHumanRequests) Deny(_ context.Context, id string) (domain.HumanRequest, error) {
	f.denied = append(f.denied, id)
	return f.request, nil
}

const humanRequestKey = "0123456789012345678901234567890123456789abc"

func humanRequestServer(t *testing.T, svc *fakeHumanRequests, verifier *uicapability.Verifier) *httptest.Server {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(httpd.NewRouterWithControl(config.Config{}, log, nil, httpd.APIDeps{HumanRequests: svc, UICapability: verifier}, httpd.ControlDeps{}))
	t.Cleanup(srv.Close)
	return srv
}

func sampleHumanRequest() domain.HumanRequest {
	at := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	return domain.HumanRequest{
		ID: "hr-1", Kind: domain.HumanRequestAutoResumeLift, SessionID: "repo-1",
		Requester: domain.HumanRequestRequester{Kind: domain.HumanRequesterSession, SessionID: "repo-2"},
		Summary:   "Allow automatic resume for Fix the bug (repo-1).", PayloadHash: "abc123",
		Changes: []domain.HumanRequestChange{{Field: "autoResume", From: "never", To: "allowed"}},
		State:   domain.HumanRequestPending, CreatedAt: at, ExpiresAt: at.Add(24 * time.Hour),
	}
}

func hrDo(t *testing.T, srv *httptest.Server, method, path, body, capability string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if capability != "" {
		req.Header.Set(uicapability.Header, capability)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func signedFor(method, path string, at time.Time) string {
	return uicapability.Sign(humanRequestKey, uicapability.PurposeHumanWrite, uicapability.RequestSubject(method, path), at)
}

func TestFilingARequestReturnsTheDaemonRenderedCardAndWhatToRelay(t *testing.T) {
	svc := &fakeHumanRequests{request: sampleHumanRequest()}
	srv := humanRequestServer(t, svc, uicapability.New(humanRequestKey))

	status, body := hrDo(t, srv, http.MethodPost, "/api/v1/human-requests", `{"kind":"conversation_settings","sessionId":"repo-1","payload":{"model":"m-b"},"requesterSessionId":"repo-2"}`, "")
	if status != http.StatusAccepted {
		t.Fatalf("status = %d, body %v", status, body)
	}
	request := body["request"].(map[string]any)
	if request["id"] != "hr-1" || request["summary"] != "Allow automatic resume for Fix the bug (repo-1)." || request["payloadHash"] != "abc123" || request["state"] != "pending" {
		t.Fatalf("response request = %v", request)
	}
	if message, _ := body["message"].(string); !strings.Contains(message, "hr-1") || !strings.Contains(message, "Do not retry") {
		t.Fatalf("message = %q", message)
	}
	got := svc.filed[0]
	if got.Kind != domain.HumanRequestConversationSettings || got.SessionID != "repo-1" || string(got.Payload) != `{"model":"m-b"}` ||
		got.Requester.Kind != domain.HumanRequesterSession || got.Requester.SessionID != "repo-2" {
		t.Fatalf("service got %+v", got)
	}

	if status, _ = hrDo(t, srv, http.MethodPost, "/api/v1/human-requests", `{"kind":"auto_resume_lift","sessionId":"repo-1"}`, ""); status != http.StatusAccepted {
		t.Fatalf("no-payload status = %d", status)
	}
	got = svc.filed[1]
	if got.Requester.Kind != domain.HumanRequesterTerminal || got.Requester.SessionID != "" || string(got.Payload) != `{}` {
		t.Fatalf("a caller naming no session must be a terminal with an empty payload: %+v", got)
	}
}

func TestFilingRefusesBodiesWithFieldsThatCouldCarryRequesterText(t *testing.T) {
	svc := &fakeHumanRequests{request: sampleHumanRequest()}
	srv := humanRequestServer(t, svc, uicapability.New(humanRequestKey))
	for _, body := range []string{
		`{"kind":"auto_resume_lift","sessionId":"repo-1","summary":"fix the build"}`,
		`{"kind":"auto_resume_lift","sessionId":"repo-1","note":"x"}`,
		`not json`,
		`{"kind":"auto_resume_lift","sessionId":"repo-1","payload":[1]}`,
		`{"kind":"conversation_settings","sessionId":"repo-1","payload":{"x":"` + strings.Repeat("a", 70<<10) + `"}}`,
	} {
		if status, _ := hrDo(t, srv, http.MethodPost, "/api/v1/human-requests", body, ""); status != http.StatusBadRequest {
			t.Errorf("body %.60q: status %d, want 400", body, status)
		}
	}
	if len(svc.filed) != 0 {
		t.Fatalf("the service saw %d refused filings", len(svc.filed))
	}
}

func TestListingDefaultsToOpenRequestsAndReadingIsOpen(t *testing.T) {
	svc := &fakeHumanRequests{request: sampleHumanRequest()}
	srv := humanRequestServer(t, svc, uicapability.New(humanRequestKey))
	for _, path := range []string{"/api/v1/human-requests", "/api/v1/human-requests?state=pending", "/api/v1/human-requests?state=all"} {
		status, body := hrDo(t, srv, http.MethodGet, path, "", "")
		requests, _ := body["requests"].([]any)
		if status != http.StatusOK || len(requests) != 1 {
			t.Fatalf("%s: status %d, body %v", path, status, body)
		}
	}
	if len(svc.listedAll) != 3 || svc.listedAll[0] || svc.listedAll[1] || !svc.listedAll[2] {
		t.Fatalf("list scopes = %v", svc.listedAll)
	}
	if status, _ := hrDo(t, srv, http.MethodGet, "/api/v1/human-requests?state=everything", "", ""); status != http.StatusBadRequest {
		t.Fatalf("an unknown state answered %d", status)
	}
	if status, _ := hrDo(t, srv, http.MethodGet, "/api/v1/human-requests/hr-1", "", ""); status != http.StatusOK {
		t.Fatalf("get answered %d", status)
	}
	svc.notFoundGet = true
	if status, _ := hrDo(t, srv, http.MethodGet, "/api/v1/human-requests/nope", "", ""); status != http.StatusNotFound {
		t.Fatalf("a missing request answered %d", status)
	}
}

func TestOnlyAVerifiedAppCapabilityForThatExactRequestCanApproveOrDeny(t *testing.T) {
	now := time.Now()
	approve := "/api/v1/human-requests/hr-1/approve"
	deny := "/api/v1/human-requests/hr-1/deny"
	body := `{"payloadHash":"abc123"}`
	otherRun := uicapability.Sign("another-run-key-another-run-key-another-run-key", uicapability.PurposeHumanWrite, uicapability.RequestSubject(http.MethodPost, approve), now)
	cases := map[string]struct {
		verifier   *uicapability.Verifier
		path       string
		capability string
		wantStatus int
	}{
		"no capability":                  {uicapability.New(humanRequestKey), approve, "", http.StatusForbidden},
		"garbage capability":             {uicapability.New(humanRequestKey), approve, "v1.1.x", http.StatusForbidden},
		"capability for another path":    {uicapability.New(humanRequestKey), approve, signedFor(http.MethodPost, deny, now), http.StatusForbidden},
		"capability for another id":      {uicapability.New(humanRequestKey), approve, signedFor(http.MethodPost, "/api/v1/human-requests/hr-2/approve", now), http.StatusForbidden},
		"capability for another verb":    {uicapability.New(humanRequestKey), approve, signedFor(http.MethodPut, approve, now), http.StatusForbidden},
		"stale capability":               {uicapability.New(humanRequestKey), approve, signedFor(http.MethodPost, approve, now.Add(-5*time.Minute)), http.StatusForbidden},
		"another run's key":              {uicapability.New(humanRequestKey), approve, otherRun, http.StatusForbidden},
		"a daemon the app did not start": {nil, approve, signedFor(http.MethodPost, approve, now), http.StatusForbidden},
		"the right capability":           {uicapability.New(humanRequestKey), approve, signedFor(http.MethodPost, approve, now), http.StatusOK},
	}
	for name, tc := range cases {
		svc := &fakeHumanRequests{request: sampleHumanRequest()}
		srv := humanRequestServer(t, svc, tc.verifier)
		status, out := hrDo(t, srv, http.MethodPost, tc.path, body, tc.capability)
		if status != tc.wantStatus {
			t.Errorf("%s: status %d, want %d (%v)", name, status, tc.wantStatus, out)
			continue
		}
		if tc.wantStatus == http.StatusForbidden {
			if out["code"] != "HUMAN_CAPABILITY_REQUIRED" || len(svc.approved) != 0 {
				t.Errorf("%s: code %v, approvals reached the service: %v", name, out["code"], svc.approved)
			}
		} else if len(svc.approved) != 1 || svc.approved[0] != "hr-1:abc123" {
			t.Errorf("%s: service saw %v", name, svc.approved)
		}
	}

	svc := &fakeHumanRequests{request: sampleHumanRequest()}
	srv := humanRequestServer(t, svc, uicapability.New(humanRequestKey))
	if status, _ := hrDo(t, srv, http.MethodPost, deny, "", ""); status != http.StatusForbidden || len(svc.denied) != 0 {
		t.Fatalf("deny without capability: %d, denials %v", status, svc.denied)
	}
	if status, _ := hrDo(t, srv, http.MethodPost, deny, "", signedFor(http.MethodPost, deny, now)); status != http.StatusOK || len(svc.denied) != 1 || svc.denied[0] != "hr-1" {
		t.Fatalf("deny with capability: %d, denials %v", status, svc.denied)
	}
}

func TestAnApprovalTheServiceRefusesKeepsItsStableCode(t *testing.T) {
	svc := &fakeHumanRequests{request: sampleHumanRequest(), approveErr: apierr.Conflict("HUMAN_REQUEST_CHANGED", "this request is not the one that was shown", nil)}
	srv := humanRequestServer(t, svc, uicapability.New(humanRequestKey))
	path := "/api/v1/human-requests/hr-1/approve"
	status, out := hrDo(t, srv, http.MethodPost, path, `{"payloadHash":"wrong"}`, signedFor(http.MethodPost, path, time.Now()))
	if status != http.StatusConflict || out["code"] != "HUMAN_REQUEST_CHANGED" {
		t.Fatalf("status %d, body %v", status, out)
	}
	if status, _ = hrDo(t, srv, http.MethodPost, path, `{"payloadHash":"x","extra":1}`, signedFor(http.MethodPost, path, time.Now())); status != http.StatusBadRequest {
		t.Fatalf("an unknown field in the approve body answered %d", status)
	}
}
