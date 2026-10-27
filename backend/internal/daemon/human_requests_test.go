package daemon

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

type countingSweeper struct{ n atomic.Int32 }

func (c *countingSweeper) Sweep(context.Context) { c.n.Add(1) }

func TestTheHumanRequestSweeperSweepsOnATickAndStopsWithItsContext(t *testing.T) {
	sweeper := &countingSweeper{}
	ctx, cancel := context.WithCancel(context.Background())
	done := startHumanRequestSweeper(ctx, sweeper, 5*time.Millisecond)
	deadline := time.After(2 * time.Second)
	for sweeper.n.Load() < 2 {
		select {
		case <-deadline:
			t.Fatalf("swept %d times, want at least 2", sweeper.n.Load())
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the sweeper did not stop with its context")
	}
	stopped := sweeper.n.Load()
	time.Sleep(30 * time.Millisecond)
	if sweeper.n.Load() != stopped {
		t.Fatal("the sweeper kept sweeping after it stopped")
	}
}
