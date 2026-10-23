package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
