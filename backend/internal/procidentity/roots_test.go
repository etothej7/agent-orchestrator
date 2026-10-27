package procidentity

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }

func rootsFixture(t *testing.T) (*SessionRoots, *clock, *int32, *[]domain.SessionRecord) {
	t.Helper()
	var lists int32
	sessions := []domain.SessionRecord{
		{ID: "worker-1", Mode: domain.SessionModeTUI, Metadata: domain.SessionMetadata{RuntimeHandleID: "pane-1"}},
		{ID: "chat-1", Mode: domain.SessionModeChat},
		{ID: "gone-1", Mode: domain.SessionModeTUI, IsTerminated: true, Metadata: domain.SessionMetadata{RuntimeHandleID: "pane-gone"}},
		{ID: "bare-1", Mode: domain.SessionModeTUI}, // no runtime handle
	}
	clk := &clock{now: t0}
	roots := NewSessionRoots(SessionRootsDeps{
		List: func(context.Context) ([]domain.SessionRecord, error) {
			atomic.AddInt32(&lists, 1)
			return sessions, nil
		},
		PaneRoots: func(_ context.Context, handle string) ([]int, error) {
			if handle == "pane-1" {
				return []int{200}, nil
			}
			return nil, nil
		},
		ChatHostPID: func(id domain.SessionID) (int, bool) { return 300, id == "chat-1" },
		Procs:       tree(),
		TTL:         5 * time.Second,
		Now:         clk.Now,
	})
	return roots, clk, &lists, &sessions
}

func TestSessionRootsAreTheProcessesTheDaemonStartedForLiveSessions(t *testing.T) {
	roots, _, _, _ := rootsFixture(t)
	got, err := roots.Roots(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[domain.SessionID]int{"worker-1": 200, "chat-1": 300}
	if len(got) != len(want) {
		t.Fatalf("roots = %+v, want exactly %v (no terminated session, no session without a process)", got, want)
	}
	for _, r := range got {
		if want[r.Session] != r.PID {
			t.Errorf("root %+v, want pid %d", r, want[r.Session])
		}
		info := tree()[r.PID]
		if !r.Start.Equal(info.Start) {
			t.Errorf("root %s start = %v, want the kernel's start time %v", r.Session, r.Start, info.Start)
		}
	}
}

// A root whose process is not in the table (it exited) is not registered.
func TestAnExitedRootIsNotRegistered(t *testing.T) {
	roots, _, _, _ := rootsFixture(t)
	roots.deps.ChatHostPID = func(domain.SessionID) (int, bool) { return 12345, true }
	got, err := roots.Roots(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range got {
		if r.Session == "chat-1" {
			t.Fatalf("an exited host was registered: %+v", r)
		}
	}
}

// Reads are served from a cache; a refresh happens at most once per TTL and never blocks a call
// that already has an answer.
func TestRootsAreCachedAndRefreshedInTheBackground(t *testing.T) {
	roots, clk, lists, _ := rootsFixture(t)
	for i := 0; i < 5; i++ {
		if _, err := roots.Roots(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if n := atomic.LoadInt32(lists); n != 1 {
		t.Fatalf("session list read %d times for 5 calls within the TTL, want 1", n)
	}
	clk.now = clk.now.Add(6 * time.Second)
	if _, err := roots.Roots(context.Background()); err != nil { // stale: answered at once, refresh scheduled
		t.Fatal(err)
	}
	roots.waitIdle()
	if n := atomic.LoadInt32(lists); n != 2 {
		t.Fatalf("session list read %d times after the TTL, want 2", n)
	}
}

// Past the maximum age a stale cache is not served: no answer is better than an old one.
func TestAnAncientCacheIsNotServedWhenRefreshFails(t *testing.T) {
	roots, clk, _, _ := rootsFixture(t)
	if _, err := roots.Roots(context.Background()); err != nil {
		t.Fatal(err)
	}
	roots.deps.List = func(context.Context) ([]domain.SessionRecord, error) { return nil, errors.New("db down") }
	clk.now = clk.now.Add(6 * time.Second)
	if _, err := roots.Roots(context.Background()); err != nil { // within max age: stale answer
		t.Fatalf("a recent cache was refused: %v", err)
	}
	roots.waitIdle()
	clk.now = clk.now.Add(2 * time.Minute)
	if got, err := roots.Roots(context.Background()); err == nil {
		t.Fatalf("a cache older than the maximum age was served: %+v", got)
	}
}
