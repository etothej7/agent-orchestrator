package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/identity"
)

// proofServer answers every /api/ call and records the identity header of each.
func proofServer(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var proofs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if !strings.HasPrefix(r.URL.Path, "/api/v1/sessions/") {
			return // the CLI's own telemetry ping
		}
		proofs = append(proofs, r.Header.Get(identity.HeaderProof))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"sessionId":"demo-1","policy":"never"}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &proofs
}

func TestCLICallsCarryTheSessionsIdentityProofFromItsEnvironment(t *testing.T) {
	cfg := setConfigEnv(t)
	srv, proofs := proofServer(t)
	writeRunFileFor(t, cfg, srv)
	proof, err := identity.NewProof()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(identity.EnvProof, proof)
	t.Setenv("AO_SESSION_ID", "coord-1")

	if _, errOut, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "session", "set-auto-resume", "demo-1", "never"); err != nil {
		t.Fatalf("set-auto-resume: %v\nstderr=%s", err, errOut)
	}
	if len(*proofs) != 1 || (*proofs)[0] != proof {
		t.Fatalf("daemon saw proof headers %q, want exactly the session's proof", *proofs)
	}
}

// Outside a session (a person's terminal, a script) there is no proof, and the CLI never invents
// or borrows one: it identifies a session only through the proof the daemon gave that session.
func TestCLICallsOutsideASessionCarryNoProof(t *testing.T) {
	cfg := setConfigEnv(t)
	srv, proofs := proofServer(t)
	writeRunFileFor(t, cfg, srv)
	t.Setenv(identity.EnvProof, "")
	t.Setenv("AO_SESSION_ID", "coord-1") // an id alone is not an identity

	if _, errOut, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "session", "set-auto-resume", "demo-1", "never"); err != nil {
		t.Fatalf("set-auto-resume: %v\nstderr=%s", err, errOut)
	}
	if len(*proofs) != 1 || (*proofs)[0] != "" {
		t.Fatalf("daemon saw proof headers %q, want none", *proofs)
	}
}
