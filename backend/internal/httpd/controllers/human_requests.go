package controllers

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	humanrequestsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/humanrequest"
)

// HumanRequestService is the controller-facing contract for pending human requests.
type HumanRequestService interface {
	File(ctx context.Context, in humanrequestsvc.FileInput) (domain.HumanRequest, error)
	List(ctx context.Context, all bool) ([]domain.HumanRequest, error)
	Get(ctx context.Context, id string) (domain.HumanRequest, error)
	Approve(ctx context.Context, id, payloadHash string) (domain.HumanRequest, error)
	Deny(ctx context.Context, id string) (domain.HumanRequest, error)
}

// HumanRequestsController owns the /human-requests routes. Anyone may file a request and read the
// list; approving or denying one is the person's alone (humanOnly).
type HumanRequestsController struct{ Svc HumanRequestService }

const maxHumanRequestBody = 64 << 10

// Register mounts the routes on r.
func (c *HumanRequestsController) Register(r chi.Router) {
	r.Post("/human-requests", c.file)
	r.Get("/human-requests", c.list)
	r.Get("/human-requests/{id}", c.get)
	r.With(humanOnly).Post("/human-requests/{id}/approve", c.approve)
	r.With(humanOnly).Post("/human-requests/{id}/deny", c.deny)
}

// HumanRequestIDParam is the path parameter of the per-request routes.
type HumanRequestIDParam struct {
	ID string `path:"id" description:"Pending request identifier."`
}

// ListHumanRequestsQuery selects which requests are listed.
type ListHumanRequestsQuery struct {
	State string `query:"state" enum:"pending,all" description:"pending (default) lists open requests; all lists the most recent of any state."`
}

// FileHumanRequestRequest asks the person to confirm a human-only action. Nothing the requester
// writes is shown to the person: the daemon renders the summary and diff from payload.
type FileHumanRequestRequest struct {
	Kind      string         `json:"kind" enum:"auto_resume_lift,conversation_settings,provider_switch" description:"What is being asked."`
	SessionID string         `json:"sessionId" description:"The session the action is about."`
	Payload   map[string]any `json:"payload,omitempty" description:"The structured action; its shape depends on kind."`
	// RequesterSessionID is attribution only until per-session proofs exist: the card says who the
	// caller claims to be, never more. Empty is a terminal with no identity.
	RequesterSessionID string `json:"requesterSessionId,omitempty" description:"The session filing the request, if any. Attribution only."`
}

// ApproveHumanRequestRequest names the exact request the person was shown.
type ApproveHumanRequestRequest struct {
	PayloadHash string `json:"payloadHash" description:"The payloadHash of the request as it was shown; anything else is refused."`
}

// HumanRequestChangeResponse is one line of the diff the daemon rendered.
type HumanRequestChangeResponse struct {
	Field string `json:"field"`
	From  string `json:"from"`
	To    string `json:"to"`
}

// HumanRequestRequesterResponse says who asked.
type HumanRequestRequesterResponse struct {
	Kind      string `json:"kind" enum:"session,terminal"`
	SessionID string `json:"sessionId,omitempty"`
}

// HumanRequestResponse is one request. Summary and changes are written by the daemon.
type HumanRequestResponse struct {
	ID          string                        `json:"id"`
	Kind        string                        `json:"kind"`
	SessionID   string                        `json:"sessionId"`
	Requester   HumanRequestRequesterResponse `json:"requester"`
	Summary     string                        `json:"summary"`
	Changes     []HumanRequestChangeResponse  `json:"changes"`
	PayloadHash string                        `json:"payloadHash"`
	State       string                        `json:"state" enum:"pending,approved,denied,expired,applied,failed"`
	Result      string                        `json:"result,omitempty"`
	CreatedAt   time.Time                     `json:"createdAt"`
	ExpiresAt   time.Time                     `json:"expiresAt"`
	DecidedAt   *time.Time                    `json:"decidedAt,omitempty"`
}

// FileHumanRequestResponse reports the filed request and the words the requester should relay.
type FileHumanRequestResponse struct {
	Request HumanRequestResponse `json:"request"`
	Message string               `json:"message"`
}

// ListHumanRequestsResponse lists requests, newest first.
type ListHumanRequestsResponse struct {
	Requests []HumanRequestResponse `json:"requests"`
}

// HumanRequestEnvelope wraps one request.
type HumanRequestEnvelope struct {
	Request HumanRequestResponse `json:"request"`
}

func humanRequestResponse(req domain.HumanRequest) HumanRequestResponse {
	changes := make([]HumanRequestChangeResponse, 0, len(req.Changes))
	for _, change := range req.Changes {
		changes = append(changes, HumanRequestChangeResponse{Field: change.Field, From: change.From, To: change.To})
	}
	out := HumanRequestResponse{
		ID: req.ID, Kind: string(req.Kind), SessionID: string(req.SessionID),
		Requester: HumanRequestRequesterResponse{Kind: string(req.Requester.Kind), SessionID: string(req.Requester.SessionID)},
		Summary:   req.Summary, Changes: changes, PayloadHash: req.PayloadHash, State: string(req.State), Result: req.Result,
		CreatedAt: req.CreatedAt, ExpiresAt: req.ExpiresAt,
	}
	if !req.DecidedAt.IsZero() {
		decided := req.DecidedAt
		out.DecidedAt = &decided
	}
	return out
}

func (c *HumanRequestsController) available(w http.ResponseWriter, r *http.Request) bool {
	if c.Svc != nil {
		return true
	}
	apispec.NotImplemented(w, r, r.Method, r.URL.Path)
	return false
}

func (c *HumanRequestsController) file(w http.ResponseWriter, r *http.Request) {
	if !c.available(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxHumanRequestBody)
	var in FileHumanRequestRequest
	if err := decodeJSONStrict(r, &in); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
		return
	}
	payload := json.RawMessage(`{}`)
	if in.Payload != nil {
		raw, err := json.Marshal(in.Payload)
		if err != nil {
			envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid payload", nil)
			return
		}
		payload = raw
	}
	requester := domain.HumanRequestRequester{Kind: domain.HumanRequesterTerminal}
	if id := strings.TrimSpace(in.RequesterSessionID); id != "" {
		requester = domain.HumanRequestRequester{Kind: domain.HumanRequesterSession, SessionID: domain.SessionID(id)}
	}
	req, err := c.Svc.File(r.Context(), humanrequestsvc.FileInput{
		Kind: domain.HumanRequestKind(in.Kind), SessionID: domain.SessionID(strings.TrimSpace(in.SessionID)), Payload: payload, Requester: requester,
	})
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusAccepted, FileHumanRequestResponse{
		Request: humanRequestResponse(req),
		Message: "Filed request " + req.ID + ". The person must approve it in the AO app; you will be told how it ends. Do not retry it.",
	})
}

func (c *HumanRequestsController) list(w http.ResponseWriter, r *http.Request) {
	if !c.available(w, r) {
		return
	}
	state := r.URL.Query().Get("state")
	if state != "" && state != "pending" && state != "all" {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_HUMAN_REQUEST_STATE", "state must be pending or all", nil)
		return
	}
	requests, err := c.Svc.List(r.Context(), state == "all")
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	out := make([]HumanRequestResponse, 0, len(requests))
	for _, req := range requests {
		out = append(out, humanRequestResponse(req))
	}
	envelope.WriteJSON(w, http.StatusOK, ListHumanRequestsResponse{Requests: out})
}

func (c *HumanRequestsController) get(w http.ResponseWriter, r *http.Request) {
	if !c.available(w, r) {
		return
	}
	req, err := c.Svc.Get(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, HumanRequestEnvelope{Request: humanRequestResponse(req)})
}

func (c *HumanRequestsController) approve(w http.ResponseWriter, r *http.Request) {
	if !c.available(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxHumanRequestBody)
	var in ApproveHumanRequestRequest
	if err := decodeJSONStrict(r, &in); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
		return
	}
	req, err := c.Svc.Approve(r.Context(), chi.URLParam(r, "id"), in.PayloadHash)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, HumanRequestEnvelope{Request: humanRequestResponse(req)})
}

func (c *HumanRequestsController) deny(w http.ResponseWriter, r *http.Request) {
	if !c.available(w, r) {
		return
	}
	req, err := c.Svc.Deny(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, HumanRequestEnvelope{Request: humanRequestResponse(req)})
}
