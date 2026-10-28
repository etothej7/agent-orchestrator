package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Captured on 2026-10-27 from `ioreg -r -k AppleClamshellState -d 4` on the MacBook this was built for,
// lid closed and docked: the two lines of the IOPMrootDomain node that mention the clamshell.
const ioregLidClosed = `  | {
  |   "AppleClamshellCausesSleep" = Yes
  |   "AppleClamshellState" = Yes
  | }
`

func TestParseClamshellState(t *testing.T) {
	for _, tt := range []struct {
		name       string
		out        string
		closed, ok bool
	}{
		{"real capture, lid closed", ioregLidClosed, true, true},
		{"lid open", "  |   \"AppleClamshellCausesSleep\" = Yes\n  |   \"AppleClamshellState\" = No\n", false, true},
		{"CausesSleep alone is not the lid state", "  |   \"AppleClamshellCausesSleep\" = Yes\n", false, false},
		{"no clamshell at all (a desktop)", "  |   \"IOPlatformUUID\" = \"x\"\n", false, false},
		{"empty", "", false, false},
		{"unexpected value", "  |   \"AppleClamshellState\" = Maybe\n", false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			closed, ok := parseClamshellState(tt.out)
			if closed != tt.closed || ok != tt.ok {
				t.Fatalf("parseClamshellState = (closed %v, known %v), want (%v, %v)", closed, ok, tt.closed, tt.ok)
			}
		})
	}
}

type fakeWakeLease struct {
	id             string
	startErr       error
	starts, stops  int
	stoppedAtStart int
}

func (f *fakeWakeLease) Start(bool) error { f.starts++; return f.startErr }
func (f *fakeWakeLease) Stop()            { f.stops++ }
func (f *fakeWakeLease) LeaseID() string  { return f.id }

// holdWorld is a scripted machine: leases the helper accepts, the daemon pid in the run file, the lid.
type holdWorld struct {
	now       time.Time
	leases    []string
	pid       int
	alive     map[int]bool
	lidClosed bool
	lidKnown  bool
	supported bool
	polls     int
	onPoll    func(w *holdWorld)
}

func newHoldWorld() *holdWorld {
	return &holdWorld{
		now: time.Unix(1_000_000, 0), leases: []string{"old-daemon-lease"}, pid: 100,
		alive: map[int]bool{100: true}, lidClosed: true, lidKnown: true, supported: true,
	}
}

func (w *holdWorld) env() wakeHoldEnv {
	return wakeHoldEnv{
		supported: func() bool { return w.supported },
		lidOpen:   func() (bool, bool) { return w.lidKnown && !w.lidClosed, w.lidKnown },
		liveLeases: func() []string {
			return append([]string(nil), w.leases...)
		},
		daemonPID: func() int { return w.pid },
		pidAlive:  func(pid int) bool { return w.alive[pid] },
		now:       func() time.Time { return w.now },
		wait: func(ctx context.Context, d time.Duration) {
			w.polls++
			w.now = w.now.Add(d)
			if w.onPoll != nil {
				w.onPoll(w)
			}
		},
		maxDuration: time.Minute,
		poll:        2 * time.Second,
	}
}

func TestRunWakeHold(t *testing.T) {
	t.Run("holds the lease while AO stays away, then lets go at the cap", func(t *testing.T) {
		w, lease := newHoldWorld(), &fakeWakeLease{id: "own"}
		w.leases = []string{"old-daemon-lease", "own"} // the helper lists our own lease too
		reason, err := runWakeHold(context.Background(), lease, w.env())
		if err != nil || reason != wakeHoldExpired {
			t.Fatalf("= %q, %v; want expired", reason, err)
		}
		if lease.starts != 1 || lease.stops != 1 {
			t.Fatalf("starts=%d stops=%d, want one lease started and withdrawn", lease.starts, lease.stops)
		}
		if w.polls != 30 {
			t.Fatalf("polled %d times, want 30 (a minute at 2s)", w.polls)
		}
	})

	t.Run("the departing daemon's lease and our own are not AO coming back", func(t *testing.T) {
		w, lease := newHoldWorld(), &fakeWakeLease{id: "own"}
		w.onPoll = func(w *holdWorld) { w.leases = []string{"own", "old-daemon-lease"}; w.pid = 100 }
		if reason, _ := runWakeHold(context.Background(), lease, w.env()); reason != wakeHoldExpired {
			t.Fatalf("reason = %q, want expired", reason)
		}
	})

	t.Run("exits when a new lease appears: AO is back", func(t *testing.T) {
		w, lease := newHoldWorld(), &fakeWakeLease{id: "own"}
		w.onPoll = func(w *holdWorld) {
			if w.polls == 3 {
				w.leases = []string{"own", "new-daemon-lease"}
			}
		}
		reason, err := runWakeHold(context.Background(), lease, w.env())
		if err != nil || reason != wakeHoldAOBack || w.polls != 3 || lease.stops != 1 {
			t.Fatalf("= %q, %v after %d polls, stops=%d; want ao back at poll 3, one stop", reason, err, w.polls, lease.stops)
		}
	})

	t.Run("exits when a new live daemon is in the run file, even with no lease", func(t *testing.T) {
		w, lease := newHoldWorld(), &fakeWakeLease{id: "own"}
		w.onPoll = func(w *holdWorld) {
			if w.polls == 2 {
				w.pid, w.alive[200] = 200, true
			}
		}
		if reason, _ := runWakeHold(context.Background(), lease, w.env()); reason != wakeHoldAOBack || w.polls != 2 {
			t.Fatalf("reason = %q after %d polls, want ao back at poll 2", reason, w.polls)
		}
	})

	t.Run("a run-file pid that is not alive is a stale file, not AO", func(t *testing.T) {
		w, lease := newHoldWorld(), &fakeWakeLease{id: "own"}
		w.onPoll = func(w *holdWorld) { w.pid = 300 } // 300 is not alive
		if reason, _ := runWakeHold(context.Background(), lease, w.env()); reason != wakeHoldExpired {
			t.Fatalf("reason = %q, want expired", reason)
		}
	})

	t.Run("stops holding once the lid is opened", func(t *testing.T) {
		w, lease := newHoldWorld(), &fakeWakeLease{id: "own"}
		w.onPoll = func(w *holdWorld) {
			if w.polls == 4 {
				w.lidClosed = false
			}
		}
		if reason, _ := runWakeHold(context.Background(), lease, w.env()); reason != wakeHoldLidOpen || w.polls != 4 || lease.stops != 1 {
			t.Fatalf("reason = %q after %d polls, stops=%d; want lid open at poll 4", reason, w.polls, lease.stops)
		}
	})

	t.Run("starts nothing when the lid is already open", func(t *testing.T) {
		w, lease := newHoldWorld(), &fakeWakeLease{id: "own"}
		w.lidClosed = false
		if reason, err := runWakeHold(context.Background(), lease, w.env()); err != nil || reason != wakeHoldLidOpen || lease.starts != 0 {
			t.Fatalf("= %q, %v, starts=%d; want lid open without starting a lease", reason, err, lease.starts)
		}
	})

	t.Run("keeps holding when the lid state cannot be read", func(t *testing.T) {
		w, lease := newHoldWorld(), &fakeWakeLease{id: "own"}
		w.lidKnown = false
		if reason, _ := runWakeHold(context.Background(), lease, w.env()); reason != wakeHoldExpired || lease.starts != 1 {
			t.Fatalf("reason = %q starts=%d, want it to hold until the cap", reason, lease.starts)
		}
	})

	t.Run("does nothing where closed-lid wake is unavailable", func(t *testing.T) {
		w, lease := newHoldWorld(), &fakeWakeLease{id: "own"}
		w.supported = false
		if reason, err := runWakeHold(context.Background(), lease, w.env()); err != nil || reason != wakeHoldUnsupported || lease.starts != 0 {
			t.Fatalf("= %q, %v, starts=%d; want unsupported without starting", reason, err, lease.starts)
		}
	})

	t.Run("reports a lease that cannot be started and never withdraws one", func(t *testing.T) {
		w, lease := newHoldWorld(), &fakeWakeLease{id: "own", startErr: errors.New("helper missing")}
		if _, err := runWakeHold(context.Background(), lease, w.env()); err == nil || lease.stops != 0 {
			t.Fatalf("err = %v, stops=%d; want the start error and no stop", err, lease.stops)
		}
	})

	t.Run("a start that wrote no lease holds nothing", func(t *testing.T) {
		w, lease := newHoldWorld(), &fakeWakeLease{id: ""} // helper not set up: Start returns nil without a lease
		if reason, err := runWakeHold(context.Background(), lease, w.env()); err != nil || reason != wakeHoldNoLease || w.polls != 0 {
			t.Fatalf("= %q, %v after %d polls; want no lease without waiting", reason, err, w.polls)
		}
	})

	t.Run("a termination signal withdraws the lease", func(t *testing.T) {
		w, lease := newHoldWorld(), &fakeWakeLease{id: "own"}
		ctx, cancel := context.WithCancel(context.Background())
		w.onPoll = func(w *holdWorld) {
			if w.polls == 2 {
				cancel()
			}
		}
		if reason, _ := runWakeHold(ctx, lease, w.env()); reason != wakeHoldCancelled || lease.stops != 1 {
			t.Fatalf("reason = %q stops=%d, want cancelled with one stop", reason, lease.stops)
		}
	})
}

// Captured on 2026-10-27 from ~/.ao/data/wake/status.json while AO's daemon and a second owner held
// the lid override (writer: frontend/native/wake-helper/main.swift).
const wakeStatusSample = `{"updatedAt":1793142164.1867719,"lidActive":true,"version":5,"blocked":{},"leases":["abd0eb8a-1710-4e0b-a02b-b40764e9b2d7","0336d8a7-ad52-48c4-af0d-74a83560c0ae"],"reason":"","active":true,"effectsBlocked":{}}`

// Captured on 2026-10-27 from ~/.ao/running.json (writer: the daemon at start).
const runFileSample = `{
  "pid": 48574,
  "port": 3001,
  "startedAt": "2026-10-28T00:03:31.47594Z",
  "owner": "app",
  "appRunId": "apprun-933fe8b1-0ddf-4b70-a7b0-6dd8edaba75c",
  "browserRuntimeAddress": "/tmp/ao-brd-48574-240d08a6131c6b19/browser.sock",
  "browserStreamAddress": "/tmp/ao-bstream-48574-d496e4ec39ae5ae4.sock"
}
`

func TestReadWakeStatusLeases(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	got := readWakeStatusLeases(write("status.json", wakeStatusSample))
	want := []string{"abd0eb8a-1710-4e0b-a02b-b40764e9b2d7", "0336d8a7-ad52-48c4-af0d-74a83560c0ae"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("leases = %v, want %v", got, want)
	}
	for name, body := range map[string]string{"garbage.json": "not json", "empty.json": "", "none.json": `{"active":true}`} {
		if got := readWakeStatusLeases(write(name, body)); len(got) != 0 {
			t.Fatalf("%s: leases = %v, want none", name, got)
		}
	}
	if got := readWakeStatusLeases(filepath.Join(dir, "missing.json")); len(got) != 0 {
		t.Fatalf("missing file: leases = %v, want none", got)
	}
}

func TestReadRunFilePID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "running.json")
	if err := os.WriteFile(path, []byte(runFileSample), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readRunFilePID(path); got != 48574 {
		t.Fatalf("pid = %d, want 48574", got)
	}
	if err := os.WriteFile(path, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readRunFilePID(path); got != 0 {
		t.Fatalf("garbage pid = %d, want 0", got)
	}
	if got := readRunFilePID(filepath.Join(dir, "missing.json")); got != 0 {
		t.Fatalf("missing pid = %d, want 0", got)
	}
}

func TestWakeHoldCommandIsHiddenAndTakesNoArguments(t *testing.T) {
	cmd := newWakeHoldCommand()
	if !cmd.Hidden {
		t.Fatal("wake-hold is an internal command and must stay out of `ao --help`")
	}
	if err := cmd.Args(cmd, []string{"extra"}); err == nil {
		t.Fatal("wake-hold must reject stray arguments")
	}
	if f := cmd.Flags().Lookup("max-duration"); f == nil || f.DefValue != wakeHoldMaxDuration.String() {
		t.Fatalf("max-duration flag = %v, want default %s", f, wakeHoldMaxDuration)
	}
}

// The desktop app starts `ao wake-hold` on quit (frontend/src/main/quit-holdover.ts, WAKE_HOLD_ARGS).
func TestRootRegistersWakeHoldWhereTheAppExpectsIt(t *testing.T) {
	root := NewRootCommand(Deps{Out: &bytes.Buffer{}, Err: &bytes.Buffer{}})
	cmd, rest, err := root.Find([]string{"wake-hold"})
	if err != nil || cmd == nil || cmd.Name() != "wake-hold" || len(rest) != 0 {
		t.Fatalf("root.Find(wake-hold) = %v, %v, %v; want the wake-hold command", cmd, rest, err)
	}
	if !cmd.Hidden {
		t.Fatal("wake-hold must stay hidden from `ao --help`")
	}
	var help bytes.Buffer
	root.SetOut(&help)
	root.SetErr(&help)
	root.SetArgs([]string{"--help"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(help.String(), "wake-hold") {
		t.Fatalf("`ao --help` lists the internal wake-hold command:\n%s", help.String())
	}
}
