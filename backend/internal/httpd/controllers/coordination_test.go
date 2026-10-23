package controllers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/service/coordination"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

func TestCoordinationHTTPSetupAndRequiredFields(t *testing.T) {
	dir := t.TempDir()
	store, err := sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	svc := coordination.New(store, dir)
	router := chi.NewRouter()
	(&SettingsController{Coordination: svc}).Register(router)
	for _, tc := range []struct {
		method, path, body string
		status             int
	}{
		{http.MethodPost, "/settings/coordination/project", `{"agent":"not-an-agent"}`, http.StatusBadRequest},
		{http.MethodPost, "/settings/coordination/project", `{"agent":"codex"}`, http.StatusOK},
		{http.MethodPut, "/settings/coordination", `{"coordinatorProjectId":"overall-coordinator","managedProjectIds":[]}`, http.StatusOK},
		{http.MethodPut, "/settings/coordination", `{}`, http.StatusBadRequest},
		{http.MethodPut, "/settings/coordination", `{"coordinatorProjectId":"","managedProjectIds":null}`, http.StatusBadRequest},
		{http.MethodGet, "/settings/coordination", "", http.StatusOK},
	} {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body)))
		if response.Code != tc.status {
			t.Fatalf("%s %s: got %d %s", tc.method, tc.path, response.Code, response.Body.String())
		}
	}
	cfg, err := svc.Get(context.Background())
	if err != nil || cfg.CoordinatorProjectID != "overall-coordinator" {
		t.Fatalf("invalid requests cleared assignments: %+v, %v", cfg, err)
	}
}
