package controllers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/identityobs"
)

type fakeObservations struct{ snap identityobs.Snapshot }

func (f fakeObservations) Snapshot(int) identityobs.Snapshot { return f.snap }

func TestIdentityObservationsReturnsTheTopOffendersWithoutSecrets(t *testing.T) {
	at := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	c := &IdentityObservationsController{
		Source: fakeObservations{snap: identityobs.Snapshot{
			Since: at, Total: 7, Allowed: 5, Refused: 2, DroppedLookups: 1,
			Routes:    []identityobs.RouteObservation{{Method: "POST", Route: "/api/v1/sessions/{sessionId}/send", Class: "A", Calls: 5, Allowed: 5, LastSeen: at}},
			Sessions:  []identityobs.SessionObservation{{Session: "worker-1", Calls: 4, LastSeen: at}},
			Processes: []identityobs.ProcessObservation{{Name: "python3", Calls: 3, LastPID: 4242, LastSeen: at}, {Name: identityobs.Unavailable, Calls: 2}},
		}},
		Mode: func() string { return "observe" },
	}
	r := chi.NewRouter()
	c.Register(r)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/identity/observations?top=5", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var got IdentityObservationsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Mode != "observe" || got.Total != 7 || got.Allowed != 5 || got.Refused != 2 || got.DroppedLookups != 1 {
		t.Fatalf("totals = %+v", got)
	}
	if len(got.Routes) != 1 || got.Routes[0].Route != "/api/v1/sessions/{sessionId}/send" || got.Routes[0].Calls != 5 {
		t.Fatalf("routes = %+v", got.Routes)
	}
	if len(got.Sessions) != 1 || got.Sessions[0].SessionID != "worker-1" || got.Sessions[0].AnonymousCalls != 4 {
		t.Fatalf("sessions = %+v", got.Sessions)
	}
	if len(got.Processes) != 2 || got.Processes[0].Name != "python3" || got.Processes[0].Pid != 4242 || got.Processes[1].Name != "unavailable" || got.Processes[1].Pid != 0 {
		t.Fatalf("processes = %+v", got.Processes)
	}
}

func TestIdentityObservationsWithoutASourceIsNotImplemented(t *testing.T) {
	r := chi.NewRouter()
	(&IdentityObservationsController{}).Register(r)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/identity/observations", nil))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status %d, want 501", rec.Code)
	}
}

func TestIdentityObservationsClampsTheTopParameter(t *testing.T) {
	var asked int
	c := &IdentityObservationsController{Source: askedSource{&asked}}
	r := chi.NewRouter()
	c.Register(r)
	for url, want := range map[string]int{"/identity/observations": 20, "/identity/observations?top=3": 3, "/identity/observations?top=0": 20, "/identity/observations?top=99999": 100, "/identity/observations?top=zzz": 20} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
		if asked != want {
			t.Errorf("%s asked for top %d, want %d", url, asked, want)
		}
	}
}

type askedSource struct{ n *int }

func (a askedSource) Snapshot(n int) identityobs.Snapshot { *a.n = n; return identityobs.Snapshot{} }
