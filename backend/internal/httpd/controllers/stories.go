package controllers

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	storysvc "github.com/aoagents/agent-orchestrator/backend/internal/service/story"
)

// StoriesController serves the coordinator workspace without altering the session board.
type StoriesController struct{ Svc *storysvc.Service }

// Register mounts the shared workspace routes.
func (c *StoriesController) Register(r chi.Router) {
	r.Get("/workspace", c.list)
	r.Post("/workspace/epics", c.epic)
	r.Post("/workspace/stories", c.create)
	r.Get("/workspace/stories/{storyId}", c.detail)
	r.Post("/workspace/commands", c.command)
}
func (c *StoriesController) available(w http.ResponseWriter, r *http.Request) bool {
	if c.Svc != nil {
		return true
	}
	apispec.NotImplemented(w, r, r.Method, r.URL.Path)
	return false
}
func (c *StoriesController) list(w http.ResponseWriter, r *http.Request) {
	if !c.available(w, r) {
		return
	}
	epics, err := c.Svc.Store.ListEpics(r.Context())
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	stories, err := c.Svc.List(r.Context())
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, WorkspaceResponse{Epics: epics, Stories: stories})
}
func (c *StoriesController) epic(w http.ResponseWriter, r *http.Request) {
	if !c.available(w, r) {
		return
	}
	var req CreateEpicRequest
	if !decodeConversationBody(w, r, &req) {
		return
	}
	if err := c.Svc.CreateEpic(r.Context(), req.Epic); err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, req)
}
func (c *StoriesController) create(w http.ResponseWriter, r *http.Request) {
	if !c.available(w, r) {
		return
	}
	var req CreateStoryRequest
	if !decodeConversationBody(w, r, &req) {
		return
	}
	if err := c.Svc.Create(r.Context(), req.Story); err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	story, err := c.Svc.Store.GetStory(r.Context(), req.Story.ID)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, CreateStoryRequest{Story: story})
}
func (c *StoriesController) detail(w http.ResponseWriter, r *http.Request) {
	if !c.available(w, r) {
		return
	}
	id := chi.URLParam(r, "storyId")
	_, err := c.Svc.Store.GetStory(r.Context(), id)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	events, err := c.Svc.Events(r.Context(), id)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	reports := []ReportResponse{}
	for i, event := range events {
		if event.Kind != "report" {
			continue
		}
		report, found, err := c.Svc.Store.GetReport(r.Context(), event.Reference)
		if err != nil {
			envelope.WriteError(w, r, err)
			return
		}
		if !found || report.StoryID != id {
			continue
		}
		events[i].RecipientSessionID = report.RecipientSessionID
		outputs := make([]ReportOutputResponse, 0, len(report.Outputs))
		for _, output := range report.Outputs {
			outputs = append(outputs, ReportOutputResponse{Kind: string(output.Kind), Reference: output.Reference, Label: output.Label})
		}
		var acknowledgedAt *time.Time
		if !report.AcknowledgedAt.IsZero() {
			at := report.AcknowledgedAt
			acknowledgedAt = &at
		}
		reports = append(reports, ReportResponse{RecipientProjectID: string(report.DeliveryProjectID()), RecipientSessionID: string(report.RecipientSessionID), DeliveryState: string(report.DeliveryState), DeliveryBatchID: report.DeliveryBatchID, DeliveryAttempts: report.DeliveryAttempts, AcknowledgedAt: acknowledgedAt, ID: report.ID, StoryID: report.StoryID, SessionID: string(report.SessionID), ProjectID: string(report.ProjectID), State: string(report.State), Note: report.Note, Message: report.Message, Outputs: outputs, CreatedAt: report.CreatedAt, RepeatCount: report.RepeatCount})
	}
	story, err := c.Svc.Store.GetStory(r.Context(), id)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, StoryDetailResponse{Story: story, Events: events, Reports: reports})
}
func (c *StoriesController) command(w http.ResponseWriter, r *http.Request) {
	if !c.available(w, r) {
		return
	}
	var req StoryCommandRequest
	if !decodeConversationBody(w, r, &req) {
		return
	}
	event, err := c.Svc.Apply(r.Context(), storysvc.Command(req))
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, StoryEventResponse{Event: event})
}
