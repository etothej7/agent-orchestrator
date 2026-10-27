//go:build darwin

package procidentity

import (
	"context"
	"net"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"
)

func TestKernelLookupPassesItsSelfTestOnThisMachine(t *testing.T) {
	if _, _, err := NewSystem(); err != nil {
		t.Fatalf("NewSystem: %v", err)
	}
}

type onlyRoots []Root

func (r onlyRoots) Roots(context.Context) ([]Root, error) { return r, nil }

// The whole chain against real sockets and real processes: a child process of this test process
// (standing in for a session's agent) calls the "daemon" over loopback; the resolver must find the
// child via the socket table, walk its real parent chain, and match this process as the
// registered root.
func TestRealChildProcessIsIdentifiedByItsAncestry(t *testing.T) {
	procs, peer, err := NewSystem()
	if err != nil {
		t.Skipf("lookup unavailable: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	serverPort := ln.Addr().(*net.TCPAddr).Port

	child := exec.Command(os.Args[0], "-test.run=TestHelperDial")
	child.Env = append(os.Environ(), "AO_PROCIDENTITY_HELPER_PORT="+strconv.Itoa(serverPort))
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	conn, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	clientPort := conn.RemoteAddr().(*net.TCPAddr).Port

	self, err := procs.Info(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	uid := self.UID
	got, err := NewResolver(procs, peer, onlyRoots{{Session: "worker-1", PID: os.Getpid(), Start: self.Start}}, uid).
		Identify(context.Background(), clientPort, serverPort)
	if err != nil || got.Session != "worker-1" || got.PID != child.Process.Pid {
		t.Fatalf("Identify = %+v, %v; want worker-1 via the child pid %d", got, err, child.Process.Pid)
	}

	// A different start time for the registered root (a recycled pid) identifies nobody.
	stale := Root{Session: "worker-1", PID: os.Getpid(), Start: self.Start.Add(-time.Hour)}
	if got, err := NewResolver(procs, peer, onlyRoots{stale}, uid).Identify(context.Background(), clientPort, serverPort); err == nil || got.Session != "" {
		t.Fatalf("a root with the wrong start time identified %+v", got)
	}
	// The same connection with no registered root identifies nobody.
	if got, err := NewResolver(procs, peer, onlyRoots{}, uid).Identify(context.Background(), clientPort, serverPort); err == nil || got.Session != "" {
		t.Fatalf("no roots identified %+v", got)
	}
}

// TestHelperDial is the child process of the test above, not a test.
func TestHelperDial(t *testing.T) {
	port := os.Getenv("AO_PROCIDENTITY_HELPER_PORT")
	if port == "" {
		t.Skip("helper process only")
	}
	c, err := net.Dial("tcp", "127.0.0.1:"+port)
	if err != nil {
		os.Exit(1)
	}
	time.Sleep(10 * time.Second)
	_ = c.Close()
}
