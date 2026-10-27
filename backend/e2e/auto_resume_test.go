//go:build !windows

package e2e

import (
	"bufio"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestFakeAutoResumeACP is launched by the disposable opencode shim below. It is a
// resume-only ACP agent (session/resume, no session/load): the model-context-only
// mode. A prompt containing HANG never finishes, so a turn can be in flight when
// the "machine" goes away. Every call is logged with the worktree directory name,
// which is the AO session id, so the test can tell sessions apart.
func TestFakeAutoResumeACP(t *testing.T) {
	if os.Getenv("AO_E2E_FAKE_RESUME_ACP") != "1" {
		return
	}
	logPath := os.Getenv("AO_E2E_ACP_CALLS")
	record := func(method, cwd, text string) {
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			os.Exit(2)
		}
		_, _ = fmt.Fprintf(f, "%s|%s|%s|pid=%d\n", method, filepath.Base(cwd), strings.ReplaceAll(text, "\n", " "), os.Getpid())
		_ = f.Close()
	}
	sessionCwd := map[string]string{}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		var frame struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Cwd       string `json:"cwd"`
				SessionID string `json:"sessionId"`
				Prompt    []struct {
					Text string `json:"text"`
				} `json:"prompt"`
			} `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &frame) != nil || len(frame.ID) == 0 {
			continue
		}
		reply := func(result string) {
			_, _ = fmt.Fprintf(os.Stdout, `{"jsonrpc":"2.0","id":%s,"result":%s}`+"\n", frame.ID, result)
		}
		background := strings.Contains(filepath.ToSlash(frame.Params.Cwd), "/background-tasks/")
		switch frame.Method {
		case "initialize":
			reply(`{"protocolVersion":1,"agentCapabilities":{"sessionCapabilities":{"resume":{}}},"authMethods":[]}`)
		case "session/new", "session/resume":
			id := frame.Params.SessionID
			if frame.Method == "session/new" {
				id = fmt.Sprintf("prov-%x", sha1.Sum([]byte(frame.Params.Cwd)))[:17]
			}
			sessionCwd[id] = frame.Params.Cwd
			if !background {
				record(frame.Method, frame.Params.Cwd, id)
			}
			if missing := os.Getenv("AO_E2E_MISSING_DIR"); frame.Method == "session/resume" && missing != "" {
				if _, err := os.Stat(filepath.Join(missing, id)); err == nil {
					_, _ = fmt.Fprintf(os.Stdout, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32002,"message":"Resource not found"}}`+"\n", frame.ID)
					continue
				}
			}
			reply(fmt.Sprintf(`{"sessionId":%q}`, id))
		case "session/prompt":
			text := ""
			for _, part := range frame.Params.Prompt {
				text += part.Text
			}
			if background {
				_, _ = fmt.Fprintf(os.Stdout, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":%q,"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"{\"title\":\"Auto resume\",\"slug\":\"auto-resume\"}"}}}}`+"\n", frame.Params.SessionID)
				reply(`{"stopReason":"end_turn"}`)
				continue
			}
			record("session/prompt", sessionCwd[frame.Params.SessionID], text)
			if strings.Contains(text, "HANG") {
				continue // never answers: the turn is in flight when the machine goes away
			}
			_, _ = fmt.Fprintf(os.Stdout, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":%q,"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"done"}}}}`+"\n", frame.Params.SessionID)
			reply(`{"stopReason":"end_turn"}`)
		default:
			reply(`{}`)
		}
	}
	os.Exit(0)
}

// autoResumeWorld is a throwaway machine: a shim provider on PATH, a call log and a
// directory of conversations the provider has "lost".
type autoResumeWorld struct {
	dataDir, calls, missing string
}

func newAutoResumeWorld(t *testing.T) *autoResumeWorld {
	t.Helper()
	binDir := t.TempDir()
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	shim := "#!/bin/sh\ncase \"$1\" in\n" +
		"  acp) exec \"$AO_E2E_TEST_BINARY\" -test.run='^TestFakeAutoResumeACP$' ;;\n" +
		"  auth) printf '0 credentials\\n' ;;\n" +
		"  --version) printf '1.0.0\\n' ;;\n" +
		"  export) if [ -e \"$AO_E2E_MISSING_DIR/$2\" ]; then printf '{\"info\":{\"id\":\"\"}}'; else printf '{\"info\":{\"id\":\"%s\"}}' \"$2\"; fi ;;\n" +
		"  *) exit 2 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(binDir, "opencode"), []byte(shim), 0o700); err != nil {
		t.Fatal(err)
	}
	w := &autoResumeWorld{dataDir: t.TempDir(), calls: filepath.Join(t.TempDir(), "provider-calls.log"), missing: t.TempDir()}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("OPENCODE_DATA_DIR", t.TempDir())
	t.Setenv("OPENCODE_CONFIG_CONTENT", "")
	t.Setenv("AO_E2E_TEST_BINARY", testBinary)
	t.Setenv("AO_E2E_FAKE_RESUME_ACP", "1")
	t.Setenv("AO_E2E_ACP_CALLS", w.calls)
	t.Setenv("AO_E2E_MISSING_DIR", w.missing)
	return w
}

// callsFor returns the provider calls made for one AO session, optionally of one method.
func (w *autoResumeWorld) callsFor(session, method string) []string {
	data, _ := os.ReadFile(w.calls)
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.SplitN(line, "|", 4)
		if len(parts) < 4 || parts[1] != session || (method != "" && parts[0] != method) {
			continue
		}
		out = append(out, line)
	}
	return out
}

func (w *autoResumeWorld) spawnWorker(t *testing.T, d *daemon, project, prompt string) string {
	t.Helper()
	return spawn(t, d, map[string]any{
		"projectId": project, "kind": "worker", "harness": "opencode", "mode": "chat", "prompt": prompt,
	}).Session.ID
}

// worktreeOf finds a session's worktree under the data dir: managed worktrees are
// named by session id.
func (w *autoResumeWorld) worktreeOf(t *testing.T, session string) string {
	t.Helper()
	var found string
	_ = filepath.Walk(filepath.Join(w.dataDir, "worktrees"), func(path string, info os.FileInfo, err error) error {
		if err == nil && info.IsDir() && filepath.Base(path) == session && found == "" {
			found = path
			return filepath.SkipDir
		}
		return nil
	})
	if found == "" {
		t.Fatalf("no worktree found for %s under %s", session, w.dataDir)
	}
	return found
}

// treeDigest fingerprints a worktree's files (names, sizes, contents) and git state.
func treeDigest(t *testing.T, dir string) string {
	t.Helper()
	var lines []string
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil { // best effort: an unreadable entry just drops out of the digest
			return nil //nolint:nilerr // intentional
		}
		if strings.Contains(path, string(filepath.Separator)+".git"+string(filepath.Separator)) {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		if info.IsDir() {
			return nil
		}
		data, _ := os.ReadFile(path)
		lines = append(lines, fmt.Sprintf("%s %d %x", rel, info.Size(), sha1.Sum(data)))
		return nil
	})
	sort.Strings(lines)
	status, _ := exec.Command("git", "-C", dir, "status", "--porcelain").Output()
	head, _ := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	return strings.Join(lines, "\n") + "\n--\n" + string(status) + string(head)
}

func processTree(pid int) []int {
	out, _ := exec.Command("pgrep", "-P", strconv.Itoa(pid)).Output()
	pids := []int{pid}
	for _, field := range strings.Fields(string(out)) {
		if child, err := strconv.Atoi(field); err == nil {
			pids = append(pids, processTree(child)...)
		}
	}
	return pids
}

// killMachine is the power-loss half of a reboot: every chat host and provider of the
// given sessions dies with no chance to write anything.
func killMachine(t *testing.T, dataDir string, sig syscall.Signal, sessions ...string) {
	t.Helper()
	var all []int
	for _, session := range sessions {
		all = append(all, processTree(persistentHostPID(t, dataDir, session))...)
	}
	for _, pid := range all {
		_ = syscall.Kill(pid, sig)
	}
	deadline := time.Now().Add(15 * time.Second)
	for _, pid := range all {
		for processAlive(pid) && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}
		if processAlive(pid) {
			t.Fatalf("process %d survived the simulated reboot", pid)
		}
	}
}

func continueTurns(s snapshot) []message {
	var out []message
	for _, m := range s.Messages {
		if m.Role == "user" && strings.HasPrefix(m.Text, "AO restarted. Continue where you left off.") {
			out = append(out, m)
		}
	}
	return out
}

func (d *daemon) setAutoResume(session, policy string) {
	d.t.Helper()
	d.mustCall("PUT", "/sessions/"+session+"/auto-resume", http.StatusOK, map[string]any{"policy": policy}, nil)
}

// autoResumeFixture is a daemon with one session mid-turn, one idle, and one that must
// never be resumed (it also carries uncommitted work in its worktree).
type autoResumeFixture struct {
	w                     *autoResumeWorld
	d                     *daemon
	project               string
	busy, idle, protected string
	stopped               string // stopped on purpose by the user: must stay exited
	protectedTree, digest string
}

func newAutoResumeFixture(t *testing.T) *autoResumeFixture {
	t.Helper()
	if testing.Short() {
		t.Skip("boots real daemons and waits out the resume settle delay")
	}
	w := newAutoResumeWorld(t)
	d := startDaemon(t, w.dataDir)
	f := &autoResumeFixture{w: w, d: d, project: seedProject(t, d, "auto-resume")}
	f.busy = w.spawnWorker(t, d, f.project, "HANG while you work on this")
	f.idle = w.spawnWorker(t, d, f.project, "Say done")
	f.protected = w.spawnWorker(t, d, f.project, "Say done")
	d.awaitConversation(f.busy, 30*time.Second, "the busy turn to run", func(s snapshot) bool {
		return len(s.Turns) == 1 && s.Turns[0].State == "running"
	})
	for _, id := range []string{f.idle, f.protected} {
		d.awaitConversation(id, 30*time.Second, "the idle turn to finish", func(s snapshot) bool {
			return len(s.Turns) == 1 && s.Turns[0].State == "completed"
		})
	}
	// A session the user stopped on purpose must never be brought back.
	f.stopped = w.spawnWorker(t, d, f.project, "Say done")
	d.awaitConversation(f.stopped, 30*time.Second, "the to-be-stopped turn to finish", func(s snapshot) bool {
		return len(s.Turns) == 1 && s.Turns[0].State == "completed"
	})
	d.mustCall("POST", "/sessions/"+f.stopped+"/exit-agent", http.StatusOK, nil, nil)
	d.awaitConversation(f.stopped, 30*time.Second, "the user-stopped session to stop", func(s snapshot) bool {
		return s.Controller == "stopped"
	})
	d.setAutoResume(f.protected, "never")
	f.protectedTree = w.worktreeOf(t, f.protected)
	if err := os.WriteFile(filepath.Join(f.protectedTree, "uncommitted.txt"), []byte("do not lose me\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.digest = treeDigest(t, f.protectedTree)
	return f
}

// assertRecovered checks the outcome every reboot flavour must reach on `d`: the busy
// session is back on the same provider conversation with ONE hidden continue turn, the
// idle one is parked and wakes on a message, and the protected one was never touched.
func (f *autoResumeFixture) assertRecovered(t *testing.T, d *daemon, wantResumes int) {
	t.Helper()
	w := f.w
	d.awaitLiveController(f.busy, 120*time.Second)
	d.awaitConversation(f.busy, 60*time.Second, "the continue turn", func(s snapshot) bool {
		return len(continueTurns(s)) >= 1
	})
	time.Sleep(3 * time.Second) // a duplicate would have shown up by now
	after := d.conversation(f.busy)
	turns := continueTurns(after)
	if len(turns) != wantResumes || turns[0].Origin != "automation" {
		t.Fatalf("continue turns = %+v, want %d automation turn(s)\n%s", turns, wantResumes, describe(after))
	}
	if got := len(w.callsFor(f.busy, "session/resume")); got != wantResumes {
		t.Fatalf("provider saw %d session/resume for the busy session, want %d:\n%v", got, wantResumes, w.callsFor(f.busy, ""))
	}
	// At the provider level: the original prompt went out exactly once (a resume never
	// replays it) and the continue prompt exactly once per resume.
	hang, cont := 0, 0
	for _, line := range w.callsFor(f.busy, "session/prompt") {
		switch {
		case strings.Contains(line, "HANG"):
			hang++
		case strings.Contains(line, "AO restarted. Continue where you left off."):
			cont++
		}
	}
	if hang != 1 || cont != wantResumes {
		t.Fatalf("provider prompts for the busy session: original %d (want 1), continue %d (want %d):\n%v", hang, cont, wantResumes, w.callsFor(f.busy, ""))
	}
	if got := len(w.callsFor(f.busy, "session/new")); got != 1 {
		t.Fatalf("provider saw %d session/new for the busy session (a fresh conversation?), want only the original: %v", got, w.callsFor(f.busy, ""))
	}
	parked := d.awaitConversation(f.idle, 90*time.Second, "the idle session to be parked", func(s snapshot) bool {
		return s.Controller == "hibernated"
	})
	if got := len(w.callsFor(f.idle, "session/resume")); got != 0 {
		t.Fatalf("an idle session was launched at boot (%d session/resume): %s", got, describe(parked))
	}
	// The user-stopped session stays stopped and nothing ran for it.
	stoppedSnap := d.conversation(f.stopped)
	if stoppedSnap.Controller != "stopped" || len(w.callsFor(f.stopped, "session/resume")) != 0 {
		t.Fatalf("a user-stopped session was brought back: controller=%q calls=%v", stoppedSnap.Controller, w.callsFor(f.stopped, ""))
	}
	var read struct {
		Session struct {
			IsTerminated bool `json:"isTerminated"`
		} `json:"session"`
	}
	d.mustCall("GET", "/sessions/"+f.protected, http.StatusOK, nil, &read)
	if read.Session.IsTerminated || len(w.callsFor(f.protected, "session/resume")) != 0 {
		t.Fatalf("a protected session was touched: terminated=%v calls=%v", read.Session.IsTerminated, w.callsFor(f.protected, ""))
	}
	if got := treeDigest(t, f.protectedTree); got != f.digest {
		t.Fatalf("a protected session's worktree changed across the reboot:\nbefore:\n%s\nafter:\n%s", f.digest, got)
	}
	send(t, d, f.idle, "Say done again", fmt.Sprintf("wake-%d", wantResumes))
	d.awaitConversation(f.idle, 60*time.Second, "the woken session to answer", func(s snapshot) bool {
		return len(s.Turns) >= 2 && s.Turns[len(s.Turns)-1].State == "completed"
	})
	if got := len(w.callsFor(f.idle, "session/resume")); got != 1 {
		t.Fatalf("waking the parked session made %d session/resume calls, want 1", got)
	}
}

func (f *autoResumeFixture) sessions() []string { return []string{f.busy, f.idle, f.protected} }

// Power loss: the daemon and every host and provider die at once.
func TestAutoResumeAfterRebootKillingEverything(t *testing.T) {
	f := newAutoResumeFixture(t)
	f.d.kill()
	killMachine(t, f.w.dataDir, syscall.SIGKILL, f.sessions()...)
	d2 := startDaemon(t, f.w.dataDir)
	f.assertRecovered(t, d2, 1)
}

// A graceful quit with the daemon going first leaves the hosts running; the machine
// then goes down. Nothing wrote "exited", so capture must come from the prior activity.
func TestAutoResumeAfterGracefulShutdownDaemonFirst(t *testing.T) {
	f := newAutoResumeFixture(t)
	f.d.stop()
	killMachine(t, f.w.dataDir, syscall.SIGTERM, f.sessions()...)
	d2 := startDaemon(t, f.w.dataDir)
	f.assertRecovered(t, d2, 1)
}

// A graceful shutdown where the hosts and providers go first: the still-running daemon
// records the exits and fails the in-flight turns during its own shutdown window, and
// the next boot must still bring the session back (and not the one stopped on purpose).
func TestAutoResumeAfterGracefulShutdownHostsFirst(t *testing.T) {
	f := newAutoResumeFixture(t)
	killMachine(t, f.w.dataDir, syscall.SIGTERM, f.sessions()...)
	// The daemon notices the provider is gone and records the exit; the cut-off turn
	// may be left running or failed depending on the host kind, and capture covers both.
	f.d.awaitConversation(f.busy, 30*time.Second, "the daemon to notice the lost host", func(s snapshot) bool {
		return s.Controller == "stopped"
	})
	f.d.stop()
	d2 := startDaemon(t, f.w.dataDir)
	f.assertRecovered(t, d2, 1)
}

// Flapping: the daemon is killed again before the first resume ran. The captured intent
// is kept, so the session is resumed once and gets one continue turn, not two.
func TestAutoResumeFlappingRestartsDoNotDuplicate(t *testing.T) {
	f := newAutoResumeFixture(t)
	f.d.kill()
	killMachine(t, f.w.dataDir, syscall.SIGKILL, f.sessions()...)
	d2 := startDaemon(t, f.w.dataDir)
	d2.kill() // inside the settle delay: nothing resumed yet
	d3 := startDaemon(t, f.w.dataDir)
	f.assertRecovered(t, d3, 1)
}
