package controllers_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
)

func (f *autoResumeSessionService) AutoResumePolicy(_ context.Context, id domain.SessionID) (domain.AutoResumePolicy, error) {
	f.id = id
	return f.policy, f.err
}

type autoResumeSessionService struct {
	*fakeSessionService
	id     domain.SessionID
	policy domain.AutoResumePolicy
	sender domain.SessionID
	err    error
}

func (f *autoResumeSessionService) SetAutoResumePolicy(_ context.Context, id domain.SessionID, policy domain.AutoResumePolicy, sender domain.SessionID) error {
	f.id, f.policy, f.sender = id, policy, sender
	return f.err
}

func TestSessionsAPI_SetAutoResume(t *testing.T) {
	svc := &autoResumeSessionService{fakeSessionService: newFakeSessionService()}
	srv := httptest.NewServer(httpd.NewRouterWithControl(config.Config{},
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil, httpd.APIDeps{Sessions: svc}, httpd.ControlDeps{}))
	t.Cleanup(srv.Close)

	body, status, _ := doRequest(t, srv, http.MethodPut, "/api/v1/sessions/ao-1/auto-resume",
		`{"policy":"never","senderSessionId":"coord-1"}`)
	if status != http.StatusOK {
		t.Fatalf("set auto-resume = %d, want 200; body=%s", status, body)
	}
	var out controllers.SessionAutoResumeResponse
	mustJSON(t, body, &out)
	if out.SessionID != "ao-1" || out.Policy != domain.AutoResumeNever {
		t.Fatalf("response = %+v", out)
	}
	if svc.id != "ao-1" || svc.policy != domain.AutoResumeNever || svc.sender != "coord-1" {
		t.Fatalf("service saw id=%q policy=%q sender=%q", svc.id, svc.policy, svc.sender)
	}

	svc.id = ""
	for name, payload := range map[string]string{
		"missing policy": `{}`,
		"bad policy":     `{"policy":"maybe"}`,
		"bad json":       `{`,
	} {
		body, status, _ = doRequest(t, srv, http.MethodPut, "/api/v1/sessions/ao-1/auto-resume", payload)
		if status != http.StatusBadRequest {
			t.Fatalf("%s = %d, want 400; body=%s", name, status, body)
		}
	}
	if svc.id != "" {
		t.Fatal("a malformed request reached the service")
	}

	svc.err = apierr.Forbidden("AUTO_RESUME_NOT_PERMITTED", "no")
	body, status, _ = doRequest(t, srv, http.MethodPut, "/api/v1/sessions/ao-1/auto-resume", `{"policy":"allowed","senderSessionId":"coord-1","confirmedInApp":true}`)
	if status != http.StatusForbidden || !strings.Contains(string(body), "AUTO_RESUME_NOT_PERMITTED") {
		t.Fatalf("refused change = %d body=%s, want 403 AUTO_RESUME_NOT_PERMITTED", status, body)
	}
}

// A service that cannot set the policy answers 501 rather than pretending to.
func TestSessionsAPI_SetAutoResumeWithoutServiceSupportIsNotImplemented(t *testing.T) {
	srv := httptest.NewServer(httpd.NewRouterWithControl(config.Config{},
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil, httpd.APIDeps{Sessions: newFakeSessionService()}, httpd.ControlDeps{}))
	t.Cleanup(srv.Close)
	body, status, _ := doRequest(t, srv, http.MethodPut, "/api/v1/sessions/ao-1/auto-resume", `{"policy":"never"}`)
	if status != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501; body=%s", status, body)
	}
}

// Lifting a protection is the app's job: a request to allow that the app did not
// confirm is refused before the service sees it, whoever claims to send it.
func TestSessionsAPI_LiftingARequiresTheAppConfirmation(t *testing.T) {
	svc := &autoResumeSessionService{fakeSessionService: newFakeSessionService()}
	srv := httptest.NewServer(httpd.NewRouterWithControl(config.Config{},
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil, httpd.APIDeps{Sessions: svc}, httpd.ControlDeps{}))
	t.Cleanup(srv.Close)
	for name, payload := range map[string]string{
		"no sender (an old CLI with --human, a script, raw HTTP)": `{"policy":"allowed"}`,
		"worker":                     `{"policy":"allowed","senderSessionId":"worker-1"}`,
		"manager":                    `{"policy":"allowed","senderSessionId":"story-manager-1"}`,
		"reviewer":                   `{"policy":"allowed","senderSessionId":"reviewer-1"}`,
		"coordinator":                `{"policy":"allowed","senderSessionId":"coord-1"}`,
		"confirmation claimed false": `{"policy":"allowed","confirmedInApp":false}`,
	} {
		svc.id = ""
		body, status, _ := doRequest(t, srv, http.MethodPut, "/api/v1/sessions/ao-1/auto-resume", payload)
		if status != http.StatusForbidden || !strings.Contains(string(body), "AUTO_RESUME_LIFT_REQUIRES_APP") {
			t.Fatalf("%s = %d body=%s, want 403 AUTO_RESUME_LIFT_REQUIRES_APP", name, status, body)
		}
		if svc.id != "" {
			t.Fatalf("%s: a refused lift reached the service", name)
		}
	}

	// The app's confirmed request lifts it, with no sender (the person).
	body, status, _ := doRequest(t, srv, http.MethodPut, "/api/v1/sessions/ao-1/auto-resume", `{"policy":"allowed","confirmedInApp":true}`)
	if status != http.StatusOK {
		t.Fatalf("app-confirmed lift = %d body=%s, want 200", status, body)
	}
	if svc.id != "ao-1" || svc.policy != domain.AutoResumeAllowed || svc.sender != "" {
		t.Fatalf("service saw id=%q policy=%q sender=%q", svc.id, svc.policy, svc.sender)
	}

	// Tightening never needs the app: any caller may set never (the manager decides who).
	svc.id = ""
	if _, status, _ := doRequest(t, srv, http.MethodPut, "/api/v1/sessions/ao-1/auto-resume", `{"policy":"never","senderSessionId":"coord-1"}`); status != http.StatusOK || svc.policy != domain.AutoResumeNever {
		t.Fatalf("tightening = %d policy=%q", status, svc.policy)
	}
}

func TestSessionsAPI_GetAutoResumeReportsThePolicy(t *testing.T) {
	svc := &autoResumeSessionService{fakeSessionService: newFakeSessionService(), policy: domain.AutoResumeNever}
	srv := httptest.NewServer(httpd.NewRouterWithControl(config.Config{},
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil, httpd.APIDeps{Sessions: svc}, httpd.ControlDeps{}))
	t.Cleanup(srv.Close)
	body, status, _ := doRequest(t, srv, http.MethodGet, "/api/v1/sessions/ao-1/auto-resume", "")
	if status != http.StatusOK {
		t.Fatalf("get = %d body=%s", status, body)
	}
	var out controllers.SessionAutoResumeResponse
	mustJSON(t, body, &out)
	if out.SessionID != "ao-1" || out.Policy != domain.AutoResumeNever {
		t.Fatalf("response = %+v", out)
	}
}
