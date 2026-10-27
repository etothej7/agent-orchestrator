package procidentity

import (
	"context"
	"errors"
	"testing"
	"time"
)

type countingIdentifier struct {
	res   Result
	err   error
	calls int
}

func (c *countingIdentifier) Identify(context.Context, int, int) (Result, error) {
	c.calls++
	return c.res, c.err
}

func newCached(inner Identifier, procs ProcTable, clk *clock, limit int) *Cached {
	return NewCached(inner, procs, CacheConfig{TTL: 2 * time.Second, NegativeTTL: time.Second, MaxEntries: limit, Now: clk.Now})
}

func TestACachedAnswerIsServedWithinTheTTLOnlyWhileTheCallerIsTheSameProcess(t *testing.T) {
	clk := &clock{now: t0}
	inner := &countingIdentifier{res: Result{Session: "worker-1", PID: 400, RootPID: 200}}
	c := newCached(inner, tree(), clk, 10)
	for i := 0; i < 3; i++ {
		if got, err := c.Identify(context.Background(), 50000, 3001); err != nil || got.Session != "worker-1" {
			t.Fatalf("Identify = %+v, %v", got, err)
		}
	}
	if inner.calls != 1 {
		t.Fatalf("inner called %d times within the TTL, want 1", inner.calls)
	}
	clk.now = clk.now.Add(3 * time.Second)
	_, _ = c.Identify(context.Background(), 50000, 3001)
	if inner.calls != 2 {
		t.Fatalf("inner called %d times after the TTL, want 2", inner.calls)
	}
}

// If the ephemeral port was reused by another process inside the TTL, the cached pid is no longer
// the same process (start time changed, or it is gone): the answer is recomputed, never reused.
func TestACachedAnswerIsDroppedWhenItsProcessIsNotTheSameProcess(t *testing.T) {
	clk := &clock{now: t0}
	procs := tree()
	inner := &countingIdentifier{res: Result{Session: "worker-1", PID: 400, RootPID: 200}}
	c := newCached(inner, procs, clk, 10)
	_, _ = c.Identify(context.Background(), 50000, 3001)

	p := procs[400]
	p.Start = p.Start.Add(time.Minute) // pid 400 is now a different process
	procs[400] = p
	_, _ = c.Identify(context.Background(), 50000, 3001)
	if inner.calls != 2 {
		t.Fatalf("a reused pid was served from the cache (inner calls %d)", inner.calls)
	}
	delete(procs, 400)
	_, _ = c.Identify(context.Background(), 50000, 3001)
	if inner.calls != 3 {
		t.Fatalf("an exited process was served from the cache (inner calls %d)", inner.calls)
	}
}

func TestRefusalsAreCachedBrieflyToBoundTheCostOfAFlood(t *testing.T) {
	clk := &clock{now: t0}
	inner := &countingIdentifier{err: ErrNotInSession}
	c := newCached(inner, tree(), clk, 10)
	for i := 0; i < 5; i++ {
		if _, err := c.Identify(context.Background(), 50000, 3001); !errors.Is(err, ErrNotInSession) {
			t.Fatalf("err = %v", err)
		}
	}
	if inner.calls != 1 {
		t.Fatalf("inner called %d times for a repeated refusal, want 1", inner.calls)
	}
	clk.now = clk.now.Add(1500 * time.Millisecond)
	_, _ = c.Identify(context.Background(), 50000, 3001)
	if inner.calls != 2 {
		t.Fatalf("a refusal outlived its negative TTL (inner calls %d)", inner.calls)
	}
}

func TestTheCacheNeverGrowsPastItsCap(t *testing.T) {
	clk := &clock{now: t0}
	inner := &countingIdentifier{err: ErrNotInSession}
	c := newCached(inner, tree(), clk, 8)
	for port := 40000; port < 40500; port++ {
		_, _ = c.Identify(context.Background(), port, 3001)
	}
	if n := c.Len(); n > 8 {
		t.Fatalf("cache holds %d entries, cap is 8", n)
	}
}
