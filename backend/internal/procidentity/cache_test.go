package procidentity

import (
	"context"
	"errors"
	"testing"
	"time"
)

type countingIdentifier struct {
	results []Result
	errs    []error
	calls   int
}

func (c *countingIdentifier) Identify(context.Context, int, int) (Result, error) {
	i := c.calls
	c.calls++
	var res Result
	var err error
	if i < len(c.results) {
		res = c.results[i]
	}
	if i < len(c.errs) {
		err = c.errs[i]
	}
	return res, err
}

func newCached(inner Identifier, clk *clock, limit int) *Cached {
	return NewCached(inner, CacheConfig{NegativeTTL: time.Second, MaxEntries: limit, Now: clk.Now})
}

// B1: a successful identification is NEVER reused. A closed connection's ports can be taken by
// another process within moments, and an answer remembered for those ports would hand that
// process the previous owner's session. The lookup is cheap enough (0.3 ms) to do every time.
func TestAPositiveAnswerIsNeverServedFromTheCache(t *testing.T) {
	clk := &clock{now: t0}
	inner := &countingIdentifier{results: []Result{
		{Session: "worker-1", PID: 400}, // an in-tree process on port 50000
		{},                              // then an outside process on the SAME port: no session
	}, errs: []error{nil, ErrNotInSession}}
	c := newCached(inner, clk, 10)
	if got, err := c.Identify(context.Background(), 50000, 3001); err != nil || got.Session != "worker-1" {
		t.Fatalf("first = %+v, %v", got, err)
	}
	got, err := c.Identify(context.Background(), 50000, 3001) // same ports, immediately
	if err == nil || got.Session != "" {
		t.Fatalf("a second connection on the same ports inherited the first one's session: %+v, %v", got, err)
	}
	if inner.calls != 2 {
		t.Fatalf("inner called %d times; a positive answer must be recomputed every time", inner.calls)
	}
}

// Refusals are cached briefly, which can only make a caller LESS identified, never more.
func TestRefusalsAreCachedBrieflyToBoundTheCostOfAFlood(t *testing.T) {
	clk := &clock{now: t0}
	inner := &countingIdentifier{errs: []error{ErrNotInSession, ErrNotInSession, ErrNotInSession}}
	c := newCached(inner, clk, 10)
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
	inner := &countingIdentifier{}
	inner.errs = make([]error, 600)
	for i := range inner.errs {
		inner.errs[i] = ErrNotInSession
	}
	c := newCached(inner, clk, 8)
	for port := 40000; port < 40500; port++ {
		_, _ = c.Identify(context.Background(), port, 3001)
	}
	if n := c.Len(); n > 8 {
		t.Fatalf("cache holds %d entries, cap is 8", n)
	}
}

type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }
