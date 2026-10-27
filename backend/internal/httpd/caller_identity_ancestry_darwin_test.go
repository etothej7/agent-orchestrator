//go:build darwin

package httpd

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"sync"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/identity"
	"github.com/aoagents/agent-orchestrator/backend/internal/procidentity"
)

type staticRoots []procidentity.Root

func (s staticRoots) Roots(context.Context) ([]procidentity.Root, error) { return s, nil }

// The whole bridge on a real HTTP server with real processes: a child of this process (standing in
// for a pre-upgrade session's agent) calls the server with NO proof, and the handler sees the
// session this process is registered as the root of. A process outside that tree (this test
// process claiming the session in headers) is nobody.
func TestRealHTTPCallFromAChildProcessIsIdentifiedByItsAncestry(t *testing.T) {
	procs, peer, err := procidentity.NewSystem()
	if err != nil {
		t.Skipf("process lookup unavailable: %v", err)
	}
	self, err := procs.Info(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	store := &proofStore{hashes: map[domain.SessionID]string{}}
	sessions := proofSessions{"worker-1": {ID: "worker-1", ProjectID: "repo", Kind: domain.KindWorker}}
	bridge := procidentity.NewResolver(procs, peer, staticRoots{{Session: "worker-1", PID: os.Getpid(), Start: self.Start}}, self.UID)
	resolver := identity.NewResolver(store, sessions).WithAncestry(bridge)

	var mu sync.Mutex
	var seen []identity.Caller
	h := callerIdentityMiddleware(resolver, slog.New(slog.NewTextHandler(io.Discard, nil)))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, identity.FromContext(r.Context()))
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	srv := httptest.NewServer(h)
	defer srv.Close()

	// The child process: a real, separate process whose parent is this test process.
	child := exec.Command(os.Args[0], "-test.run=TestHelperHTTPCall")
	child.Env = append(os.Environ(), "AO_ANCESTRY_HELPER_URL="+srv.URL)
	if out, err := child.CombinedOutput(); err != nil {
		t.Fatalf("child: %v %s", err, out)
	}
	mu.Lock()
	got := seen
	mu.Unlock()
	if len(got) != 1 || !got[0].IsBridged() || got[0].IsAgent() || got[0].Session != "worker-1" || got[0].Via != identity.ViaAncestry {
		t.Fatalf("server saw %+v, want worker-1 via ancestry", got)
	}

	// A caller that is NOT in the registered tree, whatever it claims, is nobody: register a
	// different process as worker-1's root and repeat.
	other := procidentity.Root{Session: "worker-1", PID: 1, Start: self.Start} // pid 1 is never a root
	resolver2 := identity.NewResolver(store, sessions).WithAncestry(procidentity.NewResolver(procs, peer, staticRoots{other}, self.UID))
	srv2 := httptest.NewServer(callerIdentityMiddleware(resolver2, slog.New(slog.NewTextHandler(io.Discard, nil)))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, identity.FromContext(r.Context()))
		mu.Unlock()
	})))
	defer srv2.Close()
	child2 := exec.Command(os.Args[0], "-test.run=TestHelperHTTPCall")
	child2.Env = append(os.Environ(), "AO_ANCESTRY_HELPER_URL="+srv2.URL, "AO_SESSION_ID=worker-1")
	if out, err := child2.CombinedOutput(); err != nil {
		t.Fatalf("child2: %v %s", err, out)
	}
	mu.Lock()
	last := seen[len(seen)-1]
	mu.Unlock()
	if last.Kind != identity.KindAnonymous {
		t.Fatalf("a caller outside the registered tree (claiming AO_SESSION_ID=worker-1) was identified as %+v", last)
	}
}

// TestHelperHTTPCall is the child process of the test above, not a test.
func TestHelperHTTPCall(t *testing.T) {
	url := os.Getenv("AO_ANCESTRY_HELPER_URL")
	if url == "" {
		t.Skip("helper process only")
	}
	req, _ := http.NewRequest(http.MethodPost, url+"/api/v1/sessions/worker-1/send", nil)
	if id := os.Getenv("AO_SESSION_ID"); id != "" {
		req.Header.Set("X-AO-Session-Claim", id)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
}
