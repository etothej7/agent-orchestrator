package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func autoResumeServer(t *testing.T, status int, reply string) (*httptest.Server, *[]string) {
	t.Helper()
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.HasPrefix(r.URL.Path, "/api/v1/sessions/") {
			return // the CLI's own telemetry ping
		}
		seen = append(seen, r.Method+" "+r.URL.Path+" "+string(body))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func TestSessionSetAutoResumeSendsThePolicyAndTheCallingSession(t *testing.T) {
	cfg := setConfigEnv(t)
	srv, seen := autoResumeServer(t, http.StatusOK, `{"sessionId":"demo-1","policy":"never"}`)
	writeRunFileFor(t, cfg, srv)
	t.Setenv("AO_SESSION_ID", "coord-1")

	out, errOut, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }},
		"session", "set-auto-resume", "demo-1", "NEVER")
	if err != nil {
		t.Fatalf("set-auto-resume: %v\nstderr=%s", err, errOut)
	}
	if !strings.Contains(out, "session demo-1 auto-resume: never") {
		t.Fatalf("output = %q", out)
	}
	if len(*seen) != 1 || !strings.HasPrefix((*seen)[0], "PUT /api/v1/sessions/demo-1/auto-resume ") {
		t.Fatalf("requests = %v", *seen)
	}
	var body map[string]string
	if err := json.Unmarshal([]byte(strings.SplitN((*seen)[0], " ", 3)[2]), &body); err != nil {
		t.Fatal(err)
	}
	if body["policy"] != "never" || body["senderSessionId"] != "coord-1" {
		t.Fatalf("body = %v, want policy never from coord-1", body)
	}
}

func TestSessionSetAutoResumeRejectsAnUnknownPolicyBeforeCallingTheDaemon(t *testing.T) {
	cfg := setConfigEnv(t)
	srv, seen := autoResumeServer(t, http.StatusOK, `{}`)
	writeRunFileFor(t, cfg, srv)

	_, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }},
		"session", "set-auto-resume", "demo-1", "maybe")
	if err == nil || !strings.Contains(err.Error(), "must be allowed or never") {
		t.Fatalf("error = %v, want a policy error", err)
	}
	if len(*seen) != 0 {
		t.Fatalf("an invalid policy reached the daemon: %v", *seen)
	}
}

// No command lifts a protection by itself: allowed only files a request for the person (see
// human_request_test.go), whoever runs it and whatever it inherits, and the old --human flag no longer exists.
func TestSessionSetAutoResumeHasNoHumanFlag(t *testing.T) {
	cfg := setConfigEnv(t)
	srv, seen := autoResumeServer(t, http.StatusOK, `{"sessionId":"demo-1","policy":"allowed"}`)
	writeRunFileFor(t, cfg, srv)
	t.Setenv("AO_SESSION_ID", "worker-9")

	_, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }},
		"session", "set-auto-resume", "demo-1", "allowed", "--human")
	if err == nil || !strings.Contains(err.Error(), "unknown flag") {
		t.Fatalf("error = %v, want the flag to be unknown", err)
	}
	if len(*seen) != 0 {
		t.Fatalf("a request was sent: %v", *seen)
	}
}
