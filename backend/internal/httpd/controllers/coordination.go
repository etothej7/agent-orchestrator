package controllers

import (
	"context"
	"net/http"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
)

// CoordinationService exposes coordinator setup without launching agents.
type CoordinationService interface {
	Get(context.Context) (domain.CoordinationConfig, error)
	Set(context.Context, domain.CoordinationConfig) (domain.CoordinationConfig, error)
	CreateProject(context.Context, domain.AgentHarness) (domain.ProjectID, error)
}

func (c *SettingsController) createCoordinatorProject(w http.ResponseWriter, r *http.Request) {
	if c.Coordination == nil {
		apispec.NotImplemented(w, r, "POST", "/api/v1/settings/coordination/project")
		return
	}
	var req CreateCoordinatorProjectRequest
	if !decodeConversationBody(w, r, &req) {
		return
	}
	id, err := c.Coordination.CreateProject(r.Context(), domain.AgentHarness(req.Agent))
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, CreateCoordinatorProjectResponse{ProjectID: id})
}

func (c *SettingsController) getCoordination(w http.ResponseWriter, r *http.Request) {
	if c.Coordination == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/settings/coordination")
		return
	}
	cfg, err := c.Coordination.Get(r.Context())
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, CoordinationResponse{Coordination: cfg})
}

func (c *SettingsController) setCoordination(w http.ResponseWriter, r *http.Request) {
	if c.Coordination == nil {
		apispec.NotImplemented(w, r, "PUT", "/api/v1/settings/coordination")
		return
	}
	var req UpdateCoordinationRequest
	if !decodeConversationBody(w, r, &req) {
		return
	}
	if req.CoordinatorProjectID == nil || req.ManagedProjectIDs == nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "validation", "COORDINATION_FIELDS_REQUIRED", "coordinatorProjectId and managedProjectIds are required", nil)
		return
	}
	cfg, err := c.Coordination.Set(r.Context(), domain.CoordinationConfig{CoordinatorProjectID: *req.CoordinatorProjectID, ManagedProjectIDs: *req.ManagedProjectIDs})
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, CoordinationResponse{Coordination: cfg})
}
