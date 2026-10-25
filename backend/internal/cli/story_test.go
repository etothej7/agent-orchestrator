package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStoryRequiresExplicitContextAndRetryKey(t *testing.T) {
	for _, args := range [][]string{{"story"}, {"story", "--story", "s", "--kind", "message"}, {"report", "--checkpoint", "--note", "x", "--story", "s"}} {
		t.Run(args[0], func(t *testing.T) {
			t.Setenv("AO_SESSION_ID", "worker")
			_, _, err := executeCLI(t, Deps{}, args...)
			if err == nil || ExitCode(err) != 2 {
				t.Fatalf("expected usage error, got %v", err)
			}
		})
	}
}

func TestStorySendPreservesSenderContextAndDeliveryHandle(t *testing.T) {
	for _, command := range []string{"story", "send"} {
		t.Run(command, func(t *testing.T) {
			cfg := setConfigEnv(t)
			t.Setenv("AO_SESSION_ID", "manager")
			var got storyCommandAPIRequest
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/internal/telemetry/cli-invoked":
					w.WriteHeader(http.StatusNoContent)
				case "/api/v1/workspace/stories/s":
					_, _ = io.WriteString(w, `{"story":{"revision":7}}`)
				case "/api/v1/workspace/commands":
					if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
						t.Error(err)
					}
					_, _ = io.WriteString(w, `{"event":{"id":"key"}}`)
				default:
					t.Errorf("unexpected route %s", r.URL.Path)
				}
			}))
			defer srv.Close()
			writeRunFileFor(t, cfg, srv)
			args := []string{"story", "--story", "s", "--id", "key", "--kind", "message", "--session", "worker", "--text", "direction"}
			if command == "send" {
				args = []string{"send", "--story", "s", "--client-message-id", "key", "--session", "worker", "--message", "direction"}
			}
			_, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, args...)
			if err != nil {
				t.Fatal(err)
			}
			if got.StoryID != "s" || got.ID != "key" || got.SenderSessionID != "manager" || got.RecipientSessionID != "worker" || got.Revision != 7 || got.Text != "direction" {
				t.Fatalf("lost routing identity: %+v", got)
			}
		})
	}
}

func TestAskRequiresExplicitBlockingDecisionAndRetainsIdentity(t *testing.T) {
	t.Setenv("AO_SESSION_ID", "coordinator")
	for _, args := range [][]string{{"ask"}, {"ask", "--id", "scope", "--question", "Ack"}} {
		_, _, err := executeCLI(t, Deps{}, args...)
		if err == nil || ExitCode(err) != 2 {
			t.Fatal("missing blocking reason accepted", err)
		}
	}
	cfg := setConfigEnv(t)
	var requests []humanDecisionAPIRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/telemetry/cli-invoked" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.URL.Path != "/api/v1/sessions/coordinator/conversation/human-decisions" {
			t.Error(r.URL.Path)
		}
		var req humanDecisionAPIRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		requests = append(requests, req)
		_, _ = io.WriteString(w, `{"id":"decision-scope"}`)
	}))
	defer server.Close()
	writeRunFileFor(t, cfg, server)
	for range 2 {
		_, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "ask", "--id", "scope", "--question", "Blue or Green?", "--why-blocked", "Required label unspecified.")
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(requests) != 2 || requests[0] != requests[1] || requests[0].Purpose != "scope_question" {
		t.Fatal(requests)
	}
}

func TestIntakePreservesActualHumanSourceAndPlanOnRetry(t *testing.T) {
	cfg := setConfigEnv(t)
	t.Setenv("AO_SESSION_ID", "coordinator")
	var requests []storyIntakeAPIRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/telemetry/cli-invoked" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.URL.Path != "/api/v1/workspace/intake" {
			t.Error(r.URL.Path)
		}
		var req storyIntakeAPIRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		requests = append(requests, req)
		_ = json.NewEncoder(w).Encode(req)
	}))
	defer srv.Close()
	writeRunFileFor(t, cfg, srv)
	plan := `{"id":"intake","sourceMessageId":"real-ed-message","epic":{"id":"epic","title":"Outcome"},"stories":[{"id":"story","title":"Deliverable","scope":"Verify it","projectIds":["repo"]}]}`
	for range 2 {
		out, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "intake", "--plan", plan)
		if err != nil || !strings.Contains(out, "real-ed-message") {
			t.Fatal(out, err)
		}
	}
	if len(requests) != 2 {
		t.Fatal(requests)
	}
	a, _ := json.Marshal(requests[0])
	b, _ := json.Marshal(requests[1])
	if string(a) != string(b) || requests[0].SessionID != "coordinator" {
		t.Fatal(requests)
	}
}
