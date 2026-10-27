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
func killMachine(t *testing.T, dataDir string, sessions ...string) {
	t.Helper()
	var all []int
	for _, session := range sessions {
		all = append(all, processTree(persistentHostPID(t, dataDir, session))...)
	}
	for _, pid := range all {
		_ = syscall.Kill(pid, syscall.SIGKILL)
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

func TestAutoResumeAfterRebootKillingEverything(t *testing.T) {
	if testing.Short() {
		t.Skip("boots real daemons and waits out the resume settle delay")
	}
	w := newAutoResumeWorld(t)
	d := startDaemon(t, w.dataDir)
	project := seedProject(t, d, "auto-resume")

	busy := w.spawnWorker(t, d, project, "HANG while you work on this")
	idle := w.spawnWorker(t, d, project, "Say done")
	protected := w.spawnWorker(t, d, project, "Say done")
	d.awaitConversation(busy, 30*time.Second, "the busy turn to run", func(s snapshot) bool {
		return len(s.Turns) == 1 && s.Turns[0].State == "running"
	})
	for _, id := range []string{idle, protected} {
		d.awaitConversation(id, 30*time.Second, "the idle turn to finish", func(s snapshot) bool {
			return len(s.Turns) == 1 && s.Turns[0].State == "completed"
		})
	}
	d.setAutoResume(protected, "never")
	protectedTree := w.worktreeOf(t, protected)
	if err := os.WriteFile(filepath.Join(protectedTree, "uncommitted.txt"), []byte("do not lose me\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	beforeTree := treeDigest(t, protectedTree)

	// The machine goes away: the daemon and every host and provider die at once.
	d.kill()
	killMachine(t, w.dataDir, busy, idle, protected)

	d2 := startDaemon(t, w.dataDir)

	// The session that was mid-turn comes back by itself, on the SAME provider
	// conversation (session/resume), and gets exactly one hidden continue turn.
	d2.awaitLiveController(busy, 120*time.Second)
	after := d2.awaitConversation(busy, 60*time.Second, "the continue turn to finish", func(s snapshot) bool {
		turns := continueTurns(s)
		return len(turns) == 1 && s.Controller != "stopped"
	})
	if got := continueTurns(after); len(got) != 1 || got[0].Origin != "automation" {
		t.Fatalf("continue turns = %+v, want exactly one automation turn\n%s", got, describe(after))
	}
	if got := len(w.callsFor(busy, "session/resume")); got != 1 {
		t.Fatalf("provider saw %d session/resume for the busy session, want 1:\n%v", got, w.callsFor(busy, ""))
	}
	if got := len(w.callsFor(busy, "session/new")); got != 1 {
		t.Fatalf("provider saw %d session/new for the busy session (a fresh conversation?), want only the original: %v", got, w.callsFor(busy, ""))
	}

	// The idle session is parked, not launched: nothing ran for it.
	parked := d2.awaitConversation(idle, 90*time.Second, "the idle session to be parked", func(s snapshot) bool {
		return s.Controller == "hibernated"
	})
	if got := len(w.callsFor(idle, "session/resume")); got != 0 {
		t.Fatalf("an idle session was launched at boot (%d session/resume): %s", got, describe(parked))
	}

	// Never means never: still stopped, nothing launched, worktree byte-for-byte as it was.
	time.Sleep(5 * time.Second)
	var read struct {
		Session struct {
			Status       string `json:"status"`
			IsTerminated bool   `json:"isTerminated"`
		} `json:"session"`
	}
	d2.mustCall("GET", "/sessions/"+protected, http.StatusOK, nil, &read)
	if read.Session.IsTerminated || len(w.callsFor(protected, "session/resume")) != 0 {
		t.Fatalf("a protected session was touched: terminated=%v calls=%v", read.Session.IsTerminated, w.callsFor(protected, ""))
	}
	if got := treeDigest(t, protectedTree); got != beforeTree {
		t.Fatalf("a protected session's worktree changed across the reboot:\nbefore:\n%s\nafter:\n%s", beforeTree, got)
	}

	// The parked session wakes on the next message through the ordinary path.
	send(t, d2, idle, "Say done again", "wake-1")
	d2.awaitConversation(idle, 60*time.Second, "the woken session to answer", func(s snapshot) bool {
		return len(s.Turns) == 2 && s.Turns[1].State == "completed"
	})
	if got := len(w.callsFor(idle, "session/resume")); got != 1 {
		t.Fatalf("waking the parked session made %d session/resume calls, want 1", got)
	}
}
