package daemon

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

type scriptedStanding struct {
	coordinatorCalls, managerCalls atomic.Int32
	coordinatorResults             []bool
	managerErr                     error
}

func (d *scriptedStanding) DeliverCoordinatorStandingInstructions(context.Context) (bool, error) {
	n := int(d.coordinatorCalls.Add(1))
	return d.coordinatorResults[min(n-1, len(d.coordinatorResults)-1)], nil
}

func (d *scriptedStanding) DeliverManagerStandingInstructions(context.Context) (bool, error) {
	if n := d.managerCalls.Add(1); n <= 2 && d.managerErr != nil {
		return false, d.managerErr
	}
	return true, nil
}

func TestStandingInstructionBackoffDoublesToACap(t *testing.T) {
	base, limit := 30*time.Second, 5*time.Minute
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute}
	for i, expected := range want {
		if got := standingInstructionBackoff(base, limit, i+1); got != expected {
			t.Fatalf("attempt %d waits %v, want %v", i+1, got, expected)
		}
	}
}

func TestStandingInstructionsWaitForRecoveryRetryCoordinatorOnlyUntilDeliveredAndWakeOnAssignments(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	recovered := make(chan struct{})
	events := cdc.NewBroadcaster()
	// The coordinator is missing once, then delivered; the manager attempt fails twice.
	d := &scriptedStanding{coordinatorResults: []bool{false, true}, managerErr: errors.New("uncertain send")}
	done := startStandingInstructions(ctx, recovered, events, d, 5*time.Millisecond, 20*time.Millisecond, 0, slog.New(slog.DiscardHandler))
	time.Sleep(30 * time.Millisecond)
	if d.coordinatorCalls.Load() != 0 || d.managerCalls.Load() != 0 {
		t.Fatal("delivered before provider recovery completed")
	}
	close(recovered)
	deadline := time.Now().Add(2 * time.Second)
	for d.managerCalls.Load() < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("retries stalled: coordinator %d manager %d", d.coordinatorCalls.Load(), d.managerCalls.Load())
		}
		time.Sleep(2 * time.Millisecond)
	}
	if got := d.coordinatorCalls.Load(); got != 2 {
		t.Fatalf("coordinator attempts = %d, want 2 (retry until delivered, then never again)", got)
	}
	// Quiet now: nothing runs until a story assignment changes.
	settled := d.managerCalls.Load()
	time.Sleep(60 * time.Millisecond)
	if d.managerCalls.Load() != settled {
		t.Fatal("polled while nothing changed")
	}
	events.Publish(cdc.Event{Payload: []byte(`{"workspaceChanged":"story-1"}`)})
	deadline = time.Now().Add(2 * time.Second)
	for d.managerCalls.Load() == settled {
		if time.Now().After(deadline) {
			t.Fatal("a story assignment change did not re-check the managers")
		}
		time.Sleep(2 * time.Millisecond)
	}
	events.Publish(cdc.Event{Payload: []byte(`{"activity":"active"}`)})
	if got := d.coordinatorCalls.Load(); got != 2 {
		t.Fatalf("coordinator delivered again: %d", got)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("did not stop on shutdown")
	}
}
