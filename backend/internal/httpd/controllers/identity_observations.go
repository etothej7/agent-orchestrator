package controllers

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	"github.com/aoagents/agent-orchestrator/backend/internal/identityobs"
)

// ObservationsSource is the daemon's record of calls that arrived without an identity.
type ObservationsSource interface {
	Snapshot(n int) identityobs.Snapshot
}

// IdentityObservationsController serves the top-offenders view of anonymous calls (ED8 observe
// mode): which routes, which claimed sessions and which processes still call without an
// identity proof. Read-only and free of secrets (route patterns, session ids, process names and
// pids); loopback only (the LAN listener blocks the /api/v1/identity/observations prefix).
type IdentityObservationsController struct {
	Source ObservationsSource
	// Mode reports the identity enforcement mode ("observe" or "enforce").
	Mode func() string
}

// Register mounts the observations route.
func (c *IdentityObservationsController) Register(r chi.Router) {
	r.Get("/identity/observations", c.observations)
}

const (
	defaultObservationsTop = 20
	maxObservationsTop     = 100
)

func (c *IdentityObservationsController) observations(w http.ResponseWriter, r *http.Request) {
	if c.Source == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/identity/observations")
		return
	}
	top := defaultObservationsTop
	if v, err := strconv.Atoi(r.URL.Query().Get("top")); err == nil && v > 0 {
		top = min(v, maxObservationsTop)
	}
	snap := c.Source.Snapshot(top)
	out := IdentityObservationsResponse{
		Since: snap.Since, Total: snap.Total, Allowed: snap.Allowed, Refused: snap.Refused,
		DroppedLookups: snap.DroppedLookups,
		Routes:         []IdentityRouteObservation{}, Sessions: []IdentitySessionObservation{}, Processes: []IdentityProcessObservation{},
	}
	if c.Mode != nil {
		out.Mode = c.Mode()
	}
	for _, v := range snap.Routes {
		out.Routes = append(out.Routes, IdentityRouteObservation{Method: v.Method, Route: v.Route, Class: v.Class, Calls: v.Calls, Allowed: v.Allowed, Refused: v.Refused, LastSeen: v.LastSeen})
	}
	for _, v := range snap.Sessions {
		out.Sessions = append(out.Sessions, IdentitySessionObservation{SessionID: v.Session, AnonymousCalls: v.Calls, LastSeen: v.LastSeen})
	}
	for _, v := range snap.Processes {
		out.Processes = append(out.Processes, IdentityProcessObservation{Name: v.Name, Calls: v.Calls, Pid: v.LastPID, LastSeen: v.LastSeen})
	}
	envelope.WriteJSON(w, http.StatusOK, out)
}

// IdentityObservationsResponse is the body of GET /api/v1/identity/observations.
type IdentityObservationsResponse struct {
	// Mode is the identity enforcement mode: observe (nothing identity-gated is refused beyond the
	// predicate routes) or enforce.
	Mode string `json:"mode"`
	// Since is when the counters started (daemon start).
	Since time.Time `json:"since"`
	// Total, Allowed and Refused count anonymous calls on classified routes since Since.
	Total   int `json:"total"`
	Allowed int `json:"allowed"`
	Refused int `json:"refused"`
	// DroppedLookups counts calls whose calling process was not looked up (rate limit, timeout).
	DroppedLookups int                          `json:"droppedLookups"`
	Routes         []IdentityRouteObservation   `json:"routes"`
	Sessions       []IdentitySessionObservation `json:"sessions"`
	Processes      []IdentityProcessObservation `json:"processes"`
}

// IdentityRouteObservation is one route's anonymous-call counts.
type IdentityRouteObservation struct {
	Method   string    `json:"method"`
	Route    string    `json:"route"`
	Class    string    `json:"class"`
	Calls    int       `json:"calls"`
	Allowed  int       `json:"allowed"`
	Refused  int       `json:"refused"`
	LastSeen time.Time `json:"lastSeen"`
}

// IdentitySessionObservation is the anonymous calls that claimed to come from one session (a
// claim, never an identity).
type IdentitySessionObservation struct {
	SessionID      domain.SessionID `json:"sessionId"`
	AnonymousCalls int              `json:"anonymousCalls"`
	LastSeen       time.Time        `json:"lastSeen"`
}

// IdentityProcessObservation is the anonymous calls attributed, best effort, to a process name.
// Name is "unavailable" and Pid 0 when the caller could not be looked up.
type IdentityProcessObservation struct {
	Name     string    `json:"name"`
	Calls    int       `json:"calls"`
	Pid      int       `json:"pid"`
	LastSeen time.Time `json:"lastSeen"`
}
