package controllers_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
)

type fakeSessionIdentity struct {
	mu     sync.Mutex
	proven map[domain.SessionID]bool
	calls  map[domain.SessionID]int
	asked  [][]domain.SessionID
}

func (f *fakeSessionIdentity) SessionsWithProof(_ context.Context, ids []domain.SessionID) (map[domain.SessionID]bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, append([]domain.SessionID(nil), ids...))
	out := map[domain.SessionID]bool{}
	for _, id := range ids {
		if f.proven[id] {
			out[id] = true
		}
	}
	return out, nil
}

func (f *fakeSessionIdentity) AnonymousCallsFor(id domain.SessionID) int { return f.calls[id] }

type identityBody struct {
	ID       string `json:"id"`
	Identity *struct {
		Proof          string `json:"proof"`
		AnonymousCalls int    `json:"anonymousCalls"`
	} `json:"identity"`
}

// The session list and detail say, per LIVE session, whether the daemon holds an identity proof for
// it and how many anonymous calls claimed it; the UI shows a quiet marker from that. Terminated
// sessions carry no identity field, and the list asks the store ONCE.
func TestSessionViewsCarryIdentityProofStateForLiveSessionsOnly(t *testing.T) {
	svc := newFakeSessionService()
	live, proven, dead := svc.sessions["ao-1"], svc.sessions["ao-1"], svc.sessions["ao-1"]
	live.ID, proven.ID, dead.ID = "live-1", "proven-1", "dead-1"
	dead.IsTerminated = true
	svc.sessions = map[domain.SessionID]domain.Session{"live-1": live, "proven-1": proven, "dead-1": dead}
	src := &fakeSessionIdentity{proven: map[domain.SessionID]bool{"proven-1": true}, calls: map[domain.SessionID]int{"live-1": 4, "dead-1": 9}}
	srv := newSessionIdentityServer(t, svc, src)

	body, status, _ := doRequest(t, srv, "GET", "/api/v1/sessions", "")
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}
	var list struct {
		Sessions []identityBody `json:"sessions"`
	}
	mustJSON(t, body, &list)
	byID := map[string]identityBody{}
	for _, s := range list.Sessions {
		byID[s.ID] = s
	}
	if v := byID["live-1"]; v.Identity == nil || v.Identity.Proof != "missing" || v.Identity.AnonymousCalls != 4 {
		t.Errorf("live unproven session = %+v, want proof missing, 4 anonymous calls", v.Identity)
	}
	if v := byID["proven-1"]; v.Identity == nil || v.Identity.Proof != "present" || v.Identity.AnonymousCalls != 0 {
		t.Errorf("proven session = %+v, want proof present", v.Identity)
	}
	if v := byID["dead-1"]; v.Identity != nil {
		t.Errorf("a terminated session carries identity %+v; 'missing' is meaningless there", v.Identity)
	}
	if len(src.asked) != 1 {
		t.Fatalf("the store was asked %d times for the list, want one batched query", len(src.asked))
	}
	for _, id := range src.asked[0] {
		if id == "dead-1" {
			t.Error("a terminated session was included in the batched query")
		}
	}

	one, status, _ := doRequest(t, srv, "GET", "/api/v1/sessions/live-1", "")
	if status != http.StatusOK {
		t.Fatalf("detail status %d: %s", status, one)
	}
	var detail struct {
		Session identityBody `json:"session"`
	}
	mustJSON(t, one, &detail)
	if detail.Session.Identity == nil || detail.Session.Identity.Proof != "missing" {
		t.Errorf("detail identity = %+v", detail.Session.Identity)
	}
}

func TestSessionViewsHaveNoIdentityFieldWithoutASource(t *testing.T) {
	srv := newSessionTestServer(t, newFakeSessionService())
	body, _, _ := doRequest(t, srv, "GET", "/api/v1/sessions", "")
	var raw struct {
		Sessions []map[string]any `json:"sessions"`
	}
	mustJSON(t, body, &raw)
	for _, s := range raw.Sessions {
		if _, ok := s["identity"]; ok {
			t.Fatalf("identity field present with no source: %v", s["identity"])
		}
	}
}

func newSessionIdentityServer(t *testing.T, svc *fakeSessionService, src controllers.SessionIdentitySource) *httptest.Server {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(httpd.NewRouterWithControl(config.Config{}, log, nil, httpd.APIDeps{Sessions: svc, SessionProofs: src}, httpd.ControlDeps{}))
	t.Cleanup(srv.Close)
	return srv
}
