package persistenthost

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/processalive"
)

// TestHostParentDeathHarness is the child "test process" of
// TestHostAndProviderExitWhenTheirParentTestProcessDies. It starts a detached
// host, reports the host and provider pids, and waits to be killed. It does
// nothing in a normal run.
func TestHostParentDeathHarness(t *testing.T) {
	if os.Getenv("AO_TEST_PARENT_DEATH_HARNESS") != "1" {
		return
	}
	cfg := Config{
		SessionID: "parent-death", DataDir: os.Getenv("AO_TEST_PARENT_DEATH_DATADIR"), Workdir: os.Getenv("AO_TEST_PARENT_DEATH_WORKDIR"),
		Env:  append(os.Environ(), "AO_CHAT_HOST_PROVIDER_HELPER=1"),
		Argv: []string{os.Args[0], "-test.run=TestProviderHelper"},
	}
	transport, err := ConnectOrStart(context.Background(), cfg)
	if err != nil {
		t.Fatalf("harness ConnectOrStart: %v", err)
	}
	d, err := readDescriptor(cfg.DataDir, cfg.SessionID)
	if err != nil {
		t.Fatalf("harness descriptor: %v", err)
	}
	fmt.Printf("PIDS=%d,%d\n", d.PID, requestProviderPID(t, transport, 1, "pid"))
	time.Sleep(2 * time.Minute) // the outer test kills this process long before
}

// A hard-killed test binary (SIGKILL, a timeout panic, a crash) never runs its
// cleanup. The chat-host helper it spawned is detached, so without the shared
// parent-death watchdog it would run forever with its provider.
func TestHostAndProviderExitWhenTheirParentTestProcessDies(t *testing.T) {
	dataDir, workdir := t.TempDir(), t.TempDir()                                                      // the killed harness cannot clean up after itself
	harness := exec.Command(os.Args[0], "-test.run=^TestHostParentDeathHarness$", "-test.timeout=2m") //nolint:forbidigo // child test process
	harness.Env = append(os.Environ(), "AO_TEST_PARENT_DEATH_HARNESS=1", "AO_TEST_PARENT_DEATH_DATADIR="+dataDir, "AO_TEST_PARENT_DEATH_WORKDIR="+workdir)
	stdout, err := harness.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := harness.Start(); err != nil {
		t.Fatal(err)
	}
	var pids [2]int
	t.Cleanup(func() {
		// Safety net: a failing run must not leave the helper behind either.
		_ = harness.Process.Kill()
		_ = harness.Wait()
		for _, pid := range pids {
			if pid > 0 && processalive.Alive(pid) {
				if process, findErr := os.FindProcess(pid); findErr == nil {
					_ = process.Kill()
				}
			}
		}
	})

	reported := make(chan [2]int, 1)
	go func() {
		for scanner := bufio.NewScanner(stdout); scanner.Scan(); {
			var host, provider int
			if _, scanErr := fmt.Sscanf(scanner.Text(), "PIDS=%d,%d", &host, &provider); scanErr == nil {
				reported <- [2]int{host, provider}
				return
			}
		}
	}()
	select {
	case pids = <-reported:
	case <-time.After(30 * time.Second):
		t.Fatal("harness did not report host and provider pids")
	}
	if !processalive.Alive(pids[0]) || !processalive.Alive(pids[1]) {
		t.Fatalf("host %d / provider %d not running before the parent dies", pids[0], pids[1])
	}

	// Kill the parent test process the way a hard timeout or crash would.
	if err := harness.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = harness.Wait()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if !processalive.Alive(pids[0]) && !processalive.Alive(pids[1]) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("host %d / provider %d survived their parent test process being killed", pids[0], pids[1])
}
