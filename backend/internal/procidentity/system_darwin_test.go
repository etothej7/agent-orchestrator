//go:build darwin

package procidentity

import (
	"context"
	"encoding/binary"
	"net"
	"os"
	"os/exec"
	"path/filepath"
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

// A socket record counts only when BOTH its addresses are 127.0.0.1: the same two port numbers on
// another interface are a different connection.
func TestARecordMatchesOnlyAWholeLoopbackPair(t *testing.T) {
	record := func(local, foreign [4]byte) []byte {
		buf := make([]byte, 104)
		binary.LittleEndian.PutUint32(buf[0:4], 104)
		binary.LittleEndian.PutUint32(buf[4:8], xsoInpcb)
		binary.BigEndian.PutUint16(buf[16:18], 3001)  // foreign port
		binary.BigEndian.PutUint16(buf[18:20], 50000) // local port
		copy(buf[40:44], local[:])
		copy(buf[56:60], foreign[:])
		return buf
	}
	offs := [2]int{40, 56}
	lo := [4]byte{127, 0, 0, 1}
	other := [4]byte{192, 168, 1, 7}
	for name, tc := range map[string]struct {
		buf  []byte
		want bool
	}{
		"a loopback pair":       {record(lo, lo), true},
		"another local address": {record(other, lo), false},
		"another peer address":  {record(lo, other), false},
	} {
		if got := recordIsLoopbackPair(tc.buf, 0, len(tc.buf), 50000, 3001, offs); got != tc.want {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}
	buf := record(lo, lo)
	if recordIsLoopbackPair(buf, 0, len(buf), 50001, 3001, offs) || recordIsLoopbackPair(buf, 0, len(buf), 50000, 3002, offs) {
		t.Error("a record with different ports matched")
	}
	if recordIsLoopbackPair(buf, 0, len(buf), 50000, 3001, [2]int{}) {
		t.Error("a record matched with no calibrated address offsets")
	}
}

func TestKernelInspectorReadsArgsAndTheListenerOwnerOfThisProcess(t *testing.T) {
	procs, peer, err := NewSystem()
	if err != nil {
		t.Skipf("lookup unavailable: %v", err)
	}
	insp, ok := procs.(ProcInspector)
	if !ok {
		t.Fatal("the kernel process table is not a ProcInspector")
	}
	args, err := insp.Args(os.Getpid())
	if err != nil || len(args) == 0 || filepath.Base(args[0]) != filepath.Base(os.Args[0]) {
		t.Fatalf("Args = %v, %v; want this process's argv", args, err)
	}
	all, err := insp.All()
	if err != nil || len(all) < 10 {
		t.Fatalf("All = %d processes, %v", len(all), err)
	}
	found := false
	for _, p := range all {
		if p.PID == os.Getpid() && p.PPID == os.Getppid() {
			found = true
		}
	}
	if !found {
		t.Fatal("this process is not in the kernel's process list")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	owner, err := peer.(ListenerLookup).OwnerOfListener(ln.Addr().(*net.TCPAddr).Port)
	if err != nil || owner != os.Getpid() {
		t.Fatalf("listener owner = %d, %v; want this process", owner, err)
	}
	if _, err := peer.(ListenerLookup).OwnerOfListener(1); err == nil {
		t.Fatal("a port nobody listens on had an owner")
	}
}
