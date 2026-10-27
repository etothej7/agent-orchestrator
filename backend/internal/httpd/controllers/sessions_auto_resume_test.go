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
	body, status, _ = doRequest(t, srv, http.MethodPut, "/api/v1/sessions/ao-1/auto-resume", `{"policy":"allowed","senderSessionId":"coord-1"}`)
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
