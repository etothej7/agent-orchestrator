package daemon

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/cdc"
)

type scriptedDeliverer struct {
	calls   atomic.Int32
	results []bool
	err     error
}

func (d *scriptedDeliverer) DeliverCoordinatorStandingInstructions(context.Context) (bool, error) {
	n := int(d.calls.Add(1))
	if n == 1 && d.err != nil {
		return false, d.err
	}
	return d.results[min(n-1, len(d.results)-1)], nil
}

func TestCoordinatorStandingInstructionsWaitForRecoveryRetryThenStopOnceDelivered(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	recovered := make(chan struct{})
	// First attempt is uncertain, the second finds no coordinator, the third delivers.
	d := &scriptedDeliverer{results: []bool{false, false, true}, err: errors.New("uncertain send")}
	done := startCoordinatorStandingInstructions(ctx, recovered, d, 10*time.Millisecond, slog.New(slog.DiscardHandler))
	time.Sleep(50 * time.Millisecond)
	if d.calls.Load() != 0 {
		t.Fatal("delivered before provider recovery completed")
	}
	close(recovered)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("did not stop after delivery")
	}
	if got := d.calls.Load(); got != 3 {
		t.Fatalf("calls = %d, want 3 (retry until delivered, then never again)", got)
	}
	time.Sleep(50 * time.Millisecond)
	if got := d.calls.Load(); got != 3 {
		t.Fatalf("resent after delivery: %d calls", got)
	}
}

type countingAskResolver struct{ calls atomic.Int32 }

func (c *countingAskResolver) ResolveAnsweredCoordinatorAsks(context.Context) error {
	c.calls.Add(1)
	return nil
}

func TestCoordinatorAskResolutionRunsAfterRecoveryAndOnConversationChanges(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	recovered := make(chan struct{})
	events := cdc.NewBroadcaster()
	resolver := &countingAskResolver{}
	done := startCoordinatorAskResolution(ctx, recovered, events, resolver, 5*time.Millisecond, slog.New(slog.DiscardHandler))
	time.Sleep(30 * time.Millisecond)
	if resolver.calls.Load() != 0 {
		t.Fatal("resolved before provider recovery completed")
	}
	close(recovered)
	waitFor := func(n int32) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for resolver.calls.Load() < n {
			if time.Now().After(deadline) {
				t.Fatalf("resolver ran %d times, want at least %d", resolver.calls.Load(), n)
			}
			time.Sleep(2 * time.Millisecond)
		}
	}
	waitFor(1) // startup pass covers a reply made while the daemon was down
	before := resolver.calls.Load()
	events.Publish(cdc.Event{})
	waitFor(before + 1)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("did not stop on shutdown")
	}
}
