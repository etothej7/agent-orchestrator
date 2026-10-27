package procidentity

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

var t0 = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

// fakeProcs is a process table the tests control.
type fakeProcs map[int]Proc

func (f fakeProcs) Info(pid int) (Proc, error) {
	p, ok := f[pid]
	if !ok {
		return Proc{}, ErrNoProcess
	}
	return p, nil
}

type fakePeer struct {
	pid int
	err error
}

func (f fakePeer) OwnerPID(clientPort, serverPort int) (int, error) { return f.pid, f.err }

type fakeRoots []Root

func (f fakeRoots) Roots(context.Context) ([]Root, error) { return f, nil }

const me = uint32(501)

// tree: launchd(1) -> tmux server(100) -> pane shell(200, session worker-1's registered root)
// -> agent(300) -> ao CLI(400);  and an unrelated user process(900) under launchd.
func tree() fakeProcs {
	return fakeProcs{
		1:   {PID: 1, PPID: 0, Start: t0, UID: 0},
		100: {PID: 100, PPID: 1, Start: t0.Add(1 * time.Second), UID: me},
		200: {PID: 200, PPID: 100, Start: t0.Add(2 * time.Second), UID: me, Comm: "zsh"},
		300: {PID: 300, PPID: 200, Start: t0.Add(3 * time.Second), UID: me, Comm: "claude"},
		400: {PID: 400, PPID: 300, Start: t0.Add(4 * time.Second), UID: me, Comm: "ao"},
		900: {PID: 900, PPID: 1, Start: t0.Add(5 * time.Second), UID: me, Comm: "python3"},
	}
}

func resolver(procs fakeProcs, peer fakePeer, roots ...Root) *Resolver {
	return NewResolver(procs, peer, fakeRoots(roots), me)
}

func root(session domain.SessionID, pid int, start time.Time) Root {
	return Root{Session: session, PID: pid, Start: start}
}

func TestACallerInsideASessionsProcessTreeIsThatSession(t *testing.T) {
	r := resolver(tree(), fakePeer{pid: 400}, root("worker-1", 200, t0.Add(2*time.Second)))
	got, err := r.Identify(context.Background(), 50000, 3001)
	if err != nil || got.Session != "worker-1" || got.PID != 400 || got.Comm != "ao" {
		t.Fatalf("Identify = %+v, %v; want worker-1 via pid 400 (ao)", got, err)
	}
	// The root process itself counts (its own shell calling ao directly).
	r = resolver(tree(), fakePeer{pid: 200}, root("worker-1", 200, t0.Add(2*time.Second)))
	if got, err := r.Identify(context.Background(), 50000, 3001); err != nil || got.Session != "worker-1" {
		t.Fatalf("the root itself = %+v, %v", got, err)
	}
}

// Everything below must fail closed: the caller is nobody (ErrNotInSession or a lookup error).
func TestACallerOutsideEverySessionTreeGetsNoRole(t *testing.T) {
	cases := map[string]struct {
		procs fakeProcs
		peer  fakePeer
		roots []Root
	}{
		"an unrelated process of the same user (forged AO_SESSION_ID changes nothing)": {tree(), fakePeer{pid: 900}, []Root{root("worker-1", 200, t0.Add(2*time.Second))}},
		"a recycled pid: same pid, different start time":                               {tree(), fakePeer{pid: 400}, []Root{root("worker-1", 200, t0.Add(99*time.Second))}},
		"no registered roots at all":                                                   {tree(), fakePeer{pid: 400}, nil},
		"a process reparented to launchd after its parent died": {fakeProcs{
			1:   {PID: 1, Start: t0},
			400: {PID: 400, PPID: 1, Start: t0.Add(4 * time.Second), UID: me},
		}, fakePeer{pid: 400}, []Root{root("worker-1", 200, t0.Add(2*time.Second))}},
		"the socket owner is not in the process table": {tree(), fakePeer{pid: 777}, []Root{root("worker-1", 200, t0.Add(2*time.Second))}},
		"the socket lookup failed":                     {tree(), fakePeer{err: errors.New("sysctl failed")}, []Root{root("worker-1", 200, t0.Add(2*time.Second))}},
		"a root with no pid":                           {tree(), fakePeer{pid: 400}, []Root{root("worker-1", 0, t0)}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := resolver(tc.procs, tc.peer, tc.roots...).Identify(context.Background(), 50000, 3001)
			if err == nil || got.Session != "" {
				t.Fatalf("Identify = %+v, %v; want no session and an error", got, err)
			}
		})
	}
}

func TestAProcessOfAnotherUserIsNeverASession(t *testing.T) {
	procs := tree()
	p := procs[400]
	p.UID = 999
	procs[400] = p
	if got, err := resolver(procs, fakePeer{pid: 400}, root("worker-1", 200, t0.Add(2*time.Second))).Identify(context.Background(), 1, 2); err == nil || got.Session != "" {
		t.Fatalf("another user's process was identified: %+v, %v", got, err)
	}
}

// Two sessions claiming the same lineage (nested trees, or one pid registered twice) is
// ambiguous: no session is chosen.
func TestAnAmbiguousLineageIsNobody(t *testing.T) {
	both := []Root{root("worker-1", 200, t0.Add(2*time.Second)), root("manager-1", 100, t0.Add(1*time.Second))}
	if got, err := resolver(tree(), fakePeer{pid: 400}, both...).Identify(context.Background(), 1, 2); !errors.Is(err, ErrAmbiguous) || got.Session != "" {
		t.Fatalf("nested roots = %+v, %v; want ErrAmbiguous", got, err)
	}
	same := []Root{root("worker-1", 200, t0.Add(2*time.Second)), root("worker-2", 200, t0.Add(2*time.Second))}
	if got, err := resolver(tree(), fakePeer{pid: 400}, same...).Identify(context.Background(), 1, 2); !errors.Is(err, ErrAmbiguous) || got.Session != "" {
		t.Fatalf("one pid registered by two sessions = %+v, %v; want ErrAmbiguous", got, err)
	}
	// One session registering two roots (a pane and a chat host) is fine.
	one := []Root{root("worker-1", 200, t0.Add(2*time.Second)), root("worker-1", 300, t0.Add(3*time.Second))}
	if got, err := resolver(tree(), fakePeer{pid: 400}, one...).Identify(context.Background(), 1, 2); err != nil || got.Session != "worker-1" {
		t.Fatalf("one session with two roots = %+v, %v", got, err)
	}
}

// A hop whose start time is later than its child's cannot be that child's parent: the table
// changed under the walk, so the walk is not trusted.
func TestAWalkWhoseParentStartedAfterItsChildIsNotTrusted(t *testing.T) {
	procs := tree()
	p := procs[300]
	p.Start = t0.Add(10 * time.Second) // "parent" newer than its child 400 (start +4s)
	procs[300] = p
	if got, err := resolver(procs, fakePeer{pid: 400}, root("worker-1", 200, t0.Add(2*time.Second))).Identify(context.Background(), 1, 2); err == nil || got.Session != "" {
		t.Fatalf("an inconsistent chain was trusted: %+v, %v", got, err)
	}
}

func TestTheWalkIsBounded(t *testing.T) {
	procs := fakeProcs{1: {PID: 1, Start: t0}}
	for pid := 2; pid <= 200; pid++ {
		procs[pid] = Proc{PID: pid, PPID: pid - 1, Start: t0.Add(time.Duration(pid) * time.Millisecond), UID: me}
	}
	// root at pid 2, caller at 200: 198 hops, deeper than the limit.
	if got, err := resolver(procs, fakePeer{pid: 200}, root("worker-1", 2, procs[2].Start)).Identify(context.Background(), 1, 2); err == nil || got.Session != "" {
		t.Fatalf("a chain deeper than the limit matched: %+v, %v", got, err)
	}
}

func TestIdentifyNeverTrustsAnythingTheRequestSays(t *testing.T) {
	// Identify takes only the two socket ports; there is no parameter for a claimed session.
	var _ = (*Resolver)(nil).Identify
}
