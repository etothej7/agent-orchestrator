package identityobs

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/procidentity"
)

type fakePeer struct {
	pid   int
	err   error
	delay time.Duration
	calls int
	mu    sync.Mutex
}

func (f *fakePeer) OwnerPID(int, int) (int, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	time.Sleep(f.delay)
	return f.pid, f.err
}

type fakeProcs map[int]procidentity.Proc

func (f fakeProcs) Info(pid int) (procidentity.Proc, error) {
	if p, ok := f[pid]; ok {
		return p, nil
	}
	return procidentity.Proc{}, procidentity.ErrNoProcess
}

var now = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

func newRecorder(peer PeerLookup, procs ProcTable, cfg Config) *Recorder {
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return now }
	}
	return New(peer, procs, cfg)
}

func call(route string, allowed bool, claimed domain.SessionID) Call {
	return Call{Method: "POST", Route: route, Class: "A", Claimed: claimed, Allowed: allowed, ClientPort: 50000, ServerPort: 3001}
}

func TestCountsAnonymousCallsPerRouteAndPerClaimedSession(t *testing.T) {
	r := newRecorder(nil, nil, Config{})
	for i := 0; i < 3; i++ {
		r.Record(call("/api/v1/sessions/{sessionId}/send", true, "worker-1"))
	}
	r.Record(call("/api/v1/prs/{id}/merge", false, "manager-1"))
	r.Record(call("/api/v1/prs/{id}/merge", false, ""))
	snap := r.Snapshot(10)
	if snap.Total != 5 || snap.Allowed != 3 || snap.Refused != 2 {
		t.Fatalf("totals = %+v, want 5 total, 3 allowed, 2 refused", snap)
	}
	if len(snap.Routes) != 2 || snap.Routes[0].Route != "/api/v1/sessions/{sessionId}/send" || snap.Routes[0].Allowed != 3 || snap.Routes[1].Refused != 2 {
		t.Fatalf("routes = %+v (most-called first)", snap.Routes)
	}
	bySession := map[domain.SessionID]int{}
	for _, s := range snap.Sessions {
		bySession[s.Session] = s.Calls
	}
	if bySession["worker-1"] != 3 || bySession["manager-1"] != 1 || len(bySession) != 2 {
		t.Fatalf("sessions = %v, want worker-1:3 manager-1:1 (a call with no claim is not attributed to anyone)", bySession)
	}
	if got := r.AnonymousCallsFor("worker-1"); got != 3 {
		t.Fatalf("AnonymousCallsFor = %d", got)
	}
	if got := r.AnonymousCallsFor("nobody"); got != 0 {
		t.Fatalf("AnonymousCallsFor(unknown) = %d", got)
	}
}

// The pid and process name come from a best-effort lookup of the calling socket, after the call is
// counted; they are diagnostics and never touch the counts.
func TestNamesTheCallingProcessBestEffort(t *testing.T) {
	procs := fakeProcs{4242: {PID: 4242, Comm: "python3"}}
	r := newRecorder(&fakePeer{pid: 4242}, procs, Config{})
	r.Record(call("/api/v1/sessions/{sessionId}/send", true, ""))
	r.Flush()
	snap := r.Snapshot(10)
	if len(snap.Processes) != 1 || snap.Processes[0].Name != "python3" || snap.Processes[0].Calls != 1 || snap.Processes[0].LastPID != 4242 {
		t.Fatalf("processes = %+v, want python3 pid 4242", snap.Processes)
	}
	if snap.Total != 1 {
		t.Fatalf("total = %d: the lookup changed the count", snap.Total)
	}
}

func TestAnUnavailableLookupIsReportedAsUnavailable(t *testing.T) {
	r := newRecorder(&fakePeer{err: errors.New("no such socket")}, fakeProcs{}, Config{})
	r.Record(call("/r", true, ""))
	r.Flush()
	snap := r.Snapshot(10)
	if len(snap.Processes) != 1 || snap.Processes[0].Name != Unavailable || snap.Processes[0].LastPID != 0 {
		t.Fatalf("processes = %+v, want one 'unavailable' entry", snap.Processes)
	}
	// No lookup at all (non-loopback or off macOS): also unavailable, never an error.
	r2 := newRecorder(nil, nil, Config{})
	r2.Record(call("/r", true, ""))
	r2.Flush()
	if p := r2.Snapshot(10).Processes; len(p) != 1 || p[0].Name != Unavailable {
		t.Fatalf("without a lookup: %+v", p)
	}
}

// Pre-check (a): the lookup is a sink only; the observer returns no value and its outcome cannot
// change a counter, so nothing downstream can depend on it. (Source scan in the guard tests also
// pins that only this package uses the lookup.)
func TestTheLookupNeverChangesTheCounts(t *testing.T) {
	for name, peer := range map[string]PeerLookup{"found": &fakePeer{pid: 4242}, "error": &fakePeer{err: errors.New("x")}, "none": nil} {
		r := newRecorder(peer, fakeProcs{4242: {PID: 4242, Comm: "ao"}}, Config{})
		r.Record(call("/r", false, "worker-1"))
		r.Flush()
		if s := r.Snapshot(10); s.Total != 1 || s.Refused != 1 || s.Allowed != 0 {
			t.Errorf("%s: totals %+v", name, s)
		}
	}
}

// Pre-check (b): bounded and off the request path.
func TestARecordNeverWaitsForTheLookup(t *testing.T) {
	peer := &fakePeer{pid: 1, delay: 300 * time.Millisecond}
	r := newRecorder(peer, fakeProcs{1: {PID: 1, Comm: "ao"}}, Config{LookupTimeout: 50 * time.Millisecond})
	start := time.Now()
	r.Record(call("/r", true, ""))
	if d := time.Since(start); d > 20*time.Millisecond {
		t.Fatalf("Record took %v; it must not wait on a slow lookup", d)
	}
	r.Flush()
	if p := r.Snapshot(10).Processes; len(p) != 1 || p[0].Name != Unavailable {
		t.Fatalf("a lookup past its timeout = %+v, want unavailable", p)
	}
}

func TestAFloodIsRateLimitedAndBounded(t *testing.T) {
	peer := &fakePeer{pid: 1, delay: 5 * time.Millisecond}
	r := newRecorder(peer, fakeProcs{1: {PID: 1, Comm: "ao"}}, Config{MaxLookupsPerSecond: 10, Burst: 5, MaxKeys: 8, Now: time.Now})
	for i := 0; i < 500; i++ {
		c := call("/route/"+string(rune('a'+i%26)), i%2 == 0, domain.SessionID("s-"+string(rune('a'+i%26))+string(rune('a'+i%7))))
		c.ClientPort = 40000 + i
		r.Record(c)
	}
	r.Flush()
	peer.mu.Lock()
	lookups := peer.calls
	peer.mu.Unlock()
	if lookups > 20 {
		t.Fatalf("%d lookups for 500 calls at 10/s with burst 5: the rate limit did not hold", lookups)
	}
	snap := r.Snapshot(1000)
	if snap.Total != 500 {
		t.Fatalf("total = %d: calls were dropped from the counts", snap.Total)
	}
	if len(snap.Routes) > 8 || len(snap.Sessions) > 8 {
		t.Fatalf("routes %d sessions %d exceed the key cap of 8", len(snap.Routes), len(snap.Sessions))
	}
	if snap.DroppedLookups == 0 {
		t.Fatal("dropped lookups were not counted")
	}
}

func TestSnapshotIsTopNMostCalledFirst(t *testing.T) {
	r := newRecorder(nil, nil, Config{})
	for i, n := range []int{1, 5, 3} {
		for j := 0; j < n; j++ {
			r.Record(call("/route/"+string(rune('a'+i)), true, domain.SessionID("s-"+string(rune('a'+i)))))
		}
	}
	snap := r.Snapshot(2)
	if len(snap.Routes) != 2 || snap.Routes[0].Calls != 5 || snap.Routes[1].Calls != 3 {
		t.Fatalf("routes = %+v, want the two most called, most first", snap.Routes)
	}
}

func TestRecordIsSafeConcurrently(t *testing.T) {
	r := newRecorder(&fakePeer{pid: 1}, fakeProcs{1: {PID: 1, Comm: "ao"}}, Config{Now: time.Now})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				r.Record(call("/r", j%2 == 0, "worker-1"))
				_ = r.Snapshot(5)
			}
		}()
	}
	wg.Wait()
	r.Flush()
	if got := r.Snapshot(5).Total; got != 1000 {
		t.Fatalf("total = %d, want 1000", got)
	}
}

var _ = context.Background

func TestOnLookupReportsEachOutcomeForLogging(t *testing.T) {
	var mu sync.Mutex
	type seen struct {
		route string
		pid   int
		name  string
	}
	var got []seen
	cfg := Config{OnLookup: func(c Call, pid int, name string) {
		mu.Lock()
		got = append(got, seen{c.Route, pid, name})
		mu.Unlock()
	}}
	r := newRecorder(&fakePeer{pid: 4242}, fakeProcs{4242: {PID: 4242, Comm: "ao"}}, cfg)
	r.Record(call("/a", true, ""))
	r2 := newRecorder(&fakePeer{err: errors.New("gone")}, fakeProcs{}, cfg)
	r2.Record(call("/b", false, ""))
	r.Flush()
	r2.Flush()
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("OnLookup calls = %+v", got)
	}
	found := map[string]seen{}
	for _, s := range got {
		found[s.route] = s
	}
	if found["/a"].pid != 4242 || found["/a"].name != "ao" || found["/b"].pid != 0 || found["/b"].name != Unavailable {
		t.Fatalf("outcomes = %+v", found)
	}
}
