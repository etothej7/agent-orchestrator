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
	r.Post("/workspace/intake", c.intake)
	r.Post("/workspace/stories", c.create)
	r.Get("/workspace/stories/{storyId}", c.detail)
	r.Post("/workspace/commands", c.command)
	r.Get("/workspace/stories/{storyId}/continuation", c.continuation)
	r.Post("/workspace/stories/{storyId}/continuation", c.resume)
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
	result := WorkspaceResponse{Epics: epics, Stories: stories}
	if store, ok := c.Svc.Store.(storysvc.ContinuationStore); ok {
		runner := storysvc.Continuations{Store: store, Stories: c.Svc}
		for _, row := range stories {
			if !row.Autonomous {
				continue
			}
			status, err := runner.Status(r.Context(), row.ID)
			if err != nil {
				envelope.WriteError(w, r, err)
				return
			}
			result.Continuations = append(result.Continuations, status)
		}
	}
	envelope.WriteJSON(w, http.StatusOK, result)
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
	replies, err := c.Svc.Store.StoryReplies(r.Context(), id)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	result := StoryDetailResponse{Story: story, Events: events, Reports: reports, ReplyThreads: replies}
	if store, ok := c.Svc.Store.(storysvc.ContinuationStore); ok && story.Autonomous {
		runner := storysvc.Continuations{Store: store, Stories: c.Svc}
		status, err := runner.Status(r.Context(), id)
		if err != nil {
			envelope.WriteError(w, r, err)
			return
		}
		result.Continuation = &status
	}
	envelope.WriteJSON(w, http.StatusOK, result)
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

func (c *StoriesController) continuations(w http.ResponseWriter, r *http.Request) *storysvc.Continuations {
	if !c.available(w, r) {
		return nil
	}
	store, ok := c.Svc.Store.(storysvc.ContinuationStore)
	if !ok {
		apispec.NotImplemented(w, r, r.Method, r.URL.Path)
		return nil
	}
	return &storysvc.Continuations{Store: store, Stories: c.Svc}
}
func (c *StoriesController) continuation(w http.ResponseWriter, r *http.Request) {
	runner := c.continuations(w, r)
	if runner == nil {
		return
	}
	status, err := runner.Status(r.Context(), chi.URLParam(r, "storyId"))
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, StoryContinuationResponse{Continuation: status})
}
func (c *StoriesController) resume(w http.ResponseWriter, r *http.Request) {
	runner := c.continuations(w, r)
	if runner == nil {
		return
	}
	var req ResumeStoryContinuationRequest
	if !decodeConversationBody(w, r, &req) {
		return
	}
	status, err := runner.Resume(r.Context(), chi.URLParam(r, "storyId"), req.ID, req.Revision)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, StoryContinuationResponse{Continuation: status})
}

func (c *StoriesController) intake(w http.ResponseWriter, r *http.Request) {
	if !c.available(w, r) {
		return
	}
	var req StoryIntakeRequest
	if !decodeConversationBody(w, r, &req) {
		return
	}
	input := storysvc.Intake{ID: req.ID, SourceMessageID: req.SourceMessageID, SessionID: req.SessionID, Epic: req.Epic}
	for _, item := range req.Stories {
		input.Stories = append(input.Stories, storysvc.IntakeStory(item))
	}
	if err := c.Svc.Intake(r.Context(), input); err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, req)
}
