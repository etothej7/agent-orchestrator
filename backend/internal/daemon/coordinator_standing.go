package daemon

import (
	"bytes"
	"context"
	"log/slog"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/cdc"
)

type standingInstructionDeliverer interface {
	DeliverCoordinatorStandingInstructions(context.Context) (bool, error)
	DeliverManagerStandingInstructions(context.Context) (bool, error)
}

// standingInstructionBackoff is how long to wait after the nth consecutive
// attempt that left something undelivered: base, doubling, capped.
func standingInstructionBackoff(base, limit time.Duration, failures int) time.Duration {
	wait := base
	for i := 1; i < failures && wait < limit; i++ {
		wait *= 2
	}
	return min(wait, limit)
}

// startStandingInstructions teaches an existing overall coordinator and the repo
// managers working autonomous stories their current rules in place, without
// replacing them. Every delivery is one durable message per rule revision, so it
// never resends. The coordinator is retried, with capped backoff, only until it
// has the rules. Managers are re-checked when a story's assignments change (the
// workspace change events) so one assigned later is still taught; a quiet
// safety timer covers a missed event.
func startStandingInstructions(ctx context.Context, recovered <-chan struct{}, events *cdc.Broadcaster, deliverer standingInstructionDeliverer, retry, retryLimit, safety time.Duration, log *slog.Logger) <-chan struct{} {
	done := make(chan struct{})
	wake := make(chan struct{}, 1)
	unsubscribe := events.Subscribe(func(event cdc.Event) {
		if !bytes.Contains(event.Payload, []byte("workspaceChanged")) {
			return
		}
		select {
		case wake <- struct{}{}:
		default:
		}
	})
	go func() {
		defer close(done)
		defer unsubscribe()
		select {
		case <-ctx.Done():
			return
		case <-recovered:
		}
		coordinatorDone := false
		failures := 0
		for {
			pending := false
			if !coordinatorDone {
				delivered, err := deliverer.DeliverCoordinatorStandingInstructions(ctx)
				if err != nil && ctx.Err() == nil {
					log.Warn("coordinator standing instructions deferred", "err", err)
				}
				coordinatorDone = delivered && err == nil
				pending = pending || !coordinatorDone
			}
			delivered, err := deliverer.DeliverManagerStandingInstructions(ctx)
			if err != nil && ctx.Err() == nil {
				log.Warn("manager standing instructions deferred", "err", err)
			}
			pending = pending || !delivered || err != nil
			wait := safety
			if pending {
				failures++
				wait = standingInstructionBackoff(retry, retryLimit, failures)
			} else {
				failures = 0
			}
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			case <-wake:
				timer.Stop()
				// Coalesce a burst of assignment events into one pass.
				select {
				case <-ctx.Done():
					return
				case <-time.After(min(retry, 2*time.Second)):
				}
			}
		}
	}()
	return done
}
