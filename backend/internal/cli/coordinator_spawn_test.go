package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Keep this invocation aligned with overallCoordinatorPrompt. It must pass
// actual CLI validation, resolve the repo's manager harness, and reach spawn.
func TestCoordinatorManagerSpawnCommand(t *testing.T) {
	cfg := setConfigEnv(t)
	spawned := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/projects/demo":
			_, _ = io.WriteString(w, `{"project":{"id":"demo","name":"Demo","path":"/repo/demo","config":{"orchestrator":{"agent":"codex"}}}}`)
		case "/api/v1/agents/readiness/ensure":
			_, _ = io.WriteString(w, authorizedAgentsJSON("codex"))
		case "/api/v1/sessions":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body["kind"] != "orchestrator" || body["mode"] != "chat" || body["harness"] != "codex" {
				t.Errorf("manager launch body = %+v", body)
			}
			spawned = true
			_, _ = io.WriteString(w, `{"session":{"id":"demo-1","status":"idle","displayName":"Repo manager"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	writeRunFileFor(t, cfg, srv)
	out, stderr, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }},
		"spawn", "--project", "demo", "--kind", "orchestrator", "--mode", "chat", "--name", "Repo manager")
	if err != nil || !spawned || !strings.Contains(out, "demo-1") {
		t.Fatalf("manager command failed: %v, stdout=%s, stderr=%s", err, out, stderr)
	}
}
