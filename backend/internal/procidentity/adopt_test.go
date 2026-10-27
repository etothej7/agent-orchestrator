package procidentity

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

type fakeInspector struct {
	procs fakeProcs
	args  map[int][]string
}

func (f fakeInspector) All() ([]Proc, error) {
	var out []Proc
	for _, p := range f.procs {
		out = append(out, p)
	}
	return out, nil
}

func (f fakeInspector) Args(pid int) ([]string, error) {
	if a, ok := f.args[pid]; ok {
		return a, nil
	}
	return nil, ErrNoProcess
}

type fakeListeners map[int]int

func (f fakeListeners) OwnerOfListener(port int) (int, error) {
	if pid, ok := f[port]; ok {
		return pid, nil
	}
	return 0, errors.New("no listener")
}

const adoptDataDir = "/data/ao"

// adoptWorld: tmux server 100 (child of launchd) holds worker-1's pane shell 200 -> supervisor 250;
// chat host 500 (child of launchd) runs `ao chat-host chat-1 /data/ao /work` and listens on 41000;
// rogue 900 is an unrelated same-user process; 910 is its child.
func adoptWorld() (fakeProcs, fakeInspector) {
	procs := tree()
	procs[250] = Proc{PID: 250, PPID: 200, Start: t0.Add(2500 * time.Millisecond), UID: me, Comm: "ao"}
	procs[500] = Proc{PID: 500, PPID: 1, Start: t0.Add(6 * time.Second), UID: me, Comm: "ao"}
	procs[910] = Proc{PID: 910, PPID: 900, Start: t0.Add(7 * time.Second), UID: me, Comm: "python3"}
	return procs, fakeInspector{procs: procs, args: map[int][]string{
		250: {"ao", "agent-process", "supervise", "--session", "worker-1", "--launch", "launch-7", "--", "claude"},
		500: {"ao", "chat-host", "chat-1", adoptDataDir, "/work", "--", "codex"},
	}}
}

func adoptDeps(procs fakeProcs, insp fakeInspector) AdoptDeps {
	return AdoptDeps{
		Sessions: []domain.SessionRecord{
			{ID: "worker-1", Mode: domain.SessionModeTUI, Metadata: domain.SessionMetadata{RuntimeHandleID: "pane-1", RuntimeLaunchID: "launch-7"}},
			{ID: "chat-1", Mode: domain.SessionModeChat},
			{ID: "gone-1", Mode: domain.SessionModeTUI, IsTerminated: true, Metadata: domain.SessionMetadata{RuntimeHandleID: "pane-1", RuntimeLaunchID: "launch-7"}},
		},
		DataDir:       adoptDataDir,
		TmuxServerPID: 100,
		PaneRoots:     func(context.Context, string) ([]int, error) { return []int{200}, nil },
		ChatDescriptor: func(id domain.SessionID) (int, int, bool) {
			return 500, 41000, id == "chat-1"
		},
		Procs: procs, Inspector: insp, Listeners: fakeListeners{41000: 500}, UID: me,
	}
}

func rootSessions(r FrozenRoots) map[domain.SessionID]int {
	m := map[domain.SessionID]int{}
	for _, x := range r {
		m[x.Session] = x.PID
	}
	return m
}

func TestAdoptRegistersOnlyTheProcessesTheDaemonDemonstrablyStarted(t *testing.T) {
	procs, insp := adoptWorld()
	got := rootSessions(Adopt(context.Background(), adoptDeps(procs, insp)))
	if len(got) != 2 || got["worker-1"] != 200 || got["chat-1"] != 500 {
		t.Fatalf("roots = %v, want worker-1->200 and chat-1->500 only", got)
	}
}

func TestAChatDescriptorRewrittenToARogueChildIsNotAdopted(t *testing.T) {
	procs, insp := adoptWorld()
	d := adoptDeps(procs, insp)
	d.ChatDescriptor = func(domain.SessionID) (int, int, bool) { return 910, 41000, true } // rogue child of 900
	insp.args[910] = []string{"python3", "evil.py"}
	d.Inspector = insp
	if got := rootSessions(Adopt(context.Background(), d)); got["chat-1"] != 0 {
		t.Fatalf("a rogue pid in the descriptor became chat-1's root: %v", got)
	}
}

func TestAChatHostThatLooksRightButFailsAnyOneCheckIsNotAdopted(t *testing.T) {
	for name, mutate := range map[string]func(procs fakeProcs, insp fakeInspector, d *AdoptDeps){
		"parent is not launchd": func(p fakeProcs, _ fakeInspector, _ *AdoptDeps) {
			x := p[500]
			x.PPID = 900
			p[500] = x
		},
		"another user": func(p fakeProcs, _ fakeInspector, _ *AdoptDeps) {
			x := p[500]
			x.UID = 0
			p[500] = x
		},
		"not the host subcommand": func(_ fakeProcs, i fakeInspector, _ *AdoptDeps) {
			i.args[500] = []string{"python3", "evil.py", "chat-1", adoptDataDir}
		},
		"other session's host": func(_ fakeProcs, i fakeInspector, _ *AdoptDeps) {
			i.args[500] = []string{"ao", "chat-host", "chat-2", adoptDataDir, "/work"}
		},
		"other data dir": func(_ fakeProcs, i fakeInspector, _ *AdoptDeps) {
			i.args[500] = []string{"ao", "chat-host", "chat-1", "/tmp/elsewhere", "/work"}
		},
		"descriptor port is someone else's": func(_ fakeProcs, _ fakeInspector, d *AdoptDeps) {
			d.Listeners = fakeListeners{41000: 900}
		},
		"descriptor port is not listening": func(_ fakeProcs, _ fakeInspector, d *AdoptDeps) {
			d.Listeners = fakeListeners{}
		},
	} {
		t.Run(name, func(t *testing.T) {
			procs, insp := adoptWorld()
			d := adoptDeps(procs, insp)
			mutate(procs, insp, &d)
			if got := rootSessions(Adopt(context.Background(), d)); got["chat-1"] != 0 {
				t.Fatalf("adopted despite %s: %v", name, got)
			}
		})
	}
}

func TestAPlantedSameNameTmuxSessionIsNotAdopted(t *testing.T) {
	procs, insp := adoptWorld()
	d := adoptDeps(procs, insp)
	// A rogue `tmux new-session -d -s <handle>` runs under its OWN tmux server, whose pane leader's
	// parent is not AO's server, and nothing in it is the recorded supervisor.
	procs[920] = Proc{PID: 920, PPID: 905, Start: t0.Add(8 * time.Second), UID: me, Comm: "zsh"}
	procs[905] = Proc{PID: 905, PPID: 1, Start: t0.Add(8 * time.Second), UID: me, Comm: "tmux"}
	d.PaneRoots = func(context.Context, string) ([]int, error) { return []int{920}, nil }
	if got := rootSessions(Adopt(context.Background(), d)); got["worker-1"] != 0 {
		t.Fatalf("a planted tmux session became worker-1's root: %v", got)
	}
	// Even under AO's own server (a same-user process can talk to the socket), without the exact
	// supervisor for this launch it is not adopted.
	procs[920] = Proc{PID: 920, PPID: 100, Start: t0.Add(8 * time.Second), UID: me, Comm: "zsh"}
	if got := rootSessions(Adopt(context.Background(), d)); got["worker-1"] != 0 {
		t.Fatalf("a pane without the recorded supervisor was adopted: %v", got)
	}
	// The supervisor of ANOTHER launch generation does not count.
	procs[921] = Proc{PID: 921, PPID: 920, Start: t0.Add(9 * time.Second), UID: me, Comm: "ao"}
	insp.args[921] = []string{"ao", "agent-process", "supervise", "--session", "worker-1", "--launch", "launch-6", "--", "claude"}
	d.Inspector = insp
	if got := rootSessions(Adopt(context.Background(), d)); got["worker-1"] != 0 {
		t.Fatalf("a stale launch's supervisor was accepted: %v", got)
	}
}

func TestTerminalsAreNotAdoptedWithoutAnIdentifiedServerOrLaunchID(t *testing.T) {
	procs, insp := adoptWorld()
	d := adoptDeps(procs, insp)
	d.TmuxServerPID = 0
	if got := rootSessions(Adopt(context.Background(), d)); got["worker-1"] != 0 {
		t.Fatalf("adopted without a known tmux server: %v", got)
	}
	d = adoptDeps(procs, insp)
	d.Sessions[0].Metadata.RuntimeLaunchID = ""
	if got := rootSessions(Adopt(context.Background(), d)); got["worker-1"] != 0 {
		t.Fatalf("adopted without a recorded launch generation: %v", got)
	}
}

func TestFrozenRootsNeverChange(t *testing.T) {
	procs, insp := adoptWorld()
	frozen := Adopt(context.Background(), adoptDeps(procs, insp))
	n := len(frozen)
	procs[300] = Proc{PID: 300, PPID: 910, UID: me} // the world changes afterwards
	got, err := frozen.Roots(context.Background())
	if err != nil || len(got) != n {
		t.Fatalf("frozen roots changed: %v %v", got, err)
	}
}
