package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type seenRequest struct {
	method, path string
	body         map[string]any
}

// humanRequestServer answers POST /human-requests as the daemon would and records every other
// session call, so a test can prove that nothing was changed directly.
func humanRequestServer(t *testing.T) (*httptest.Server, *[]seenRequest) {
	t.Helper()
	var seen []seenRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if !strings.HasPrefix(r.URL.Path, "/api/v1/") || strings.HasPrefix(r.URL.Path, "/api/v1/internal/") {
			return // the CLI's own telemetry ping
		}
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		seen = append(seen, seenRequest{r.Method, r.URL.Path, body})
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/human-requests" {
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, `{"request":{"id":"hr-7","state":"pending","summary":"Allow automatic resume for Fix the bug (demo-1)."},"message":"Filed request hr-7. The person must approve it in the AO app."}`)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func runHumanRequestCLI(t *testing.T, agentSession string, args ...string) (string, []seenRequest, error) {
	t.Helper()
	cfg := setConfigEnv(t)
	srv, seen := humanRequestServer(t)
	writeRunFileFor(t, cfg, srv)
	t.Setenv("AO_SESSION_ID", agentSession)
	stdout, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, args...)
	return stdout, *seen, err
}

func TestSetAutoResumeAllowedFilesARequestAndNeverLiftsDirectly(t *testing.T) {
	for name, env := range map[string]string{
		"a person's terminal":             "",
		"a shell inside an agent session": "worker-9",
		"the coordinator":                 "coord-1",
	} {
		out, seen, err := runHumanRequestCLI(t, env, "session", "set-auto-resume", "demo-1", "allowed")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(seen) != 1 || seen[0].method != http.MethodPost || seen[0].path != "/api/v1/human-requests" {
			t.Fatalf("%s: calls = %+v; a lift must only ever file a request", name, seen)
		}
		body := seen[0].body
		if body["kind"] != "auto_resume_lift" || body["sessionId"] != "demo-1" || (env != "" && body["requesterSessionId"] != env) {
			t.Fatalf("%s: request body = %v", name, body)
		}
		if env == "" {
			if _, set := body["requesterSessionId"]; set {
				t.Fatalf("%s: a terminal named a requester: %v", name, body)
			}
		}
		if !strings.Contains(out, "filed request hr-7") || !strings.Contains(out, "approve it in the AO app") {
			t.Fatalf("%s: output = %q", name, out)
		}
	}
}

func TestSetAutoResumeNeverStillProtectsDirectly(t *testing.T) {
	_, seen, err := runHumanRequestCLI(t, "coord-1", "session", "set-auto-resume", "demo-1", "never")
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0].method != http.MethodPut || seen[0].path != "/api/v1/sessions/demo-1/auto-resume" || seen[0].body["policy"] != "never" {
		t.Fatalf("calls = %+v", seen)
	}
}

func TestSetModelFilesOnlyTheFieldsNamed(t *testing.T) {
	_, seen, err := runHumanRequestCLI(t, "mgr-1", "session", "set-model", "demo-1", "--model", "m-b", "--effort", "medium", "--plan=false")
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0].path != "/api/v1/human-requests" {
		t.Fatalf("calls = %+v", seen)
	}
	body := seen[0].body
	payload, _ := body["payload"].(map[string]any)
	if body["kind"] != "conversation_settings" || body["sessionId"] != "demo-1" || body["requesterSessionId"] != "mgr-1" ||
		payload["model"] != "m-b" || payload["reasoningEffort"] != "medium" || payload["planMode"] != false {
		t.Fatalf("body = %v", body)
	}
	if _, set := payload["fastMode"]; set {
		t.Fatalf("a flag that was not given was sent: %v", payload)
	}
}

func TestSetModelNeedsSomethingToChangeAndCallsNothingOtherwise(t *testing.T) {
	_, seen, err := runHumanRequestCLI(t, "mgr-1", "session", "set-model", "demo-1")
	if err == nil || !strings.Contains(err.Error(), "at least one of") {
		t.Fatalf("err = %v", err)
	}
	if len(seen) != 0 {
		t.Fatalf("calls = %+v", seen)
	}
}

func TestSwitchAgentFilesAProviderSwitchAndRequiresTheAgent(t *testing.T) {
	_, seen, err := runHumanRequestCLI(t, "mgr-1", "session", "switch-agent", "demo-1", "--agent", "codex", "--model", "o-big", "--fast")
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := seen[0].body["payload"].(map[string]any)
	if seen[0].body["kind"] != "provider_switch" || payload["targetHarness"] != "codex" || payload["model"] != "o-big" || payload["fastMode"] != true {
		t.Fatalf("body = %v", seen[0].body)
	}
	if _, set := payload["effort"]; set {
		t.Fatalf("a flag that was not given was sent: %v", payload)
	}
	_, seen, err = runHumanRequestCLI(t, "mgr-1", "session", "switch-agent", "demo-1")
	if err == nil || !strings.Contains(err.Error(), "--agent is required") || len(seen) != 0 {
		t.Fatalf("err = %v, calls %+v", err, seen)
	}
}
