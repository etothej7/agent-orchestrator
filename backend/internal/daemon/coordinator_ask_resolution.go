package daemon

import (
	"context"
	"log/slog"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/cdc"
)

type coordinatorAskResolver interface {
	ResolveAnsweredCoordinatorAsks(context.Context) error
}

// startCoordinatorAskResolution closes a coordinator question's notification as
// soon as Ed has written in that conversation, however he did it (a message, a
// queued message, a steer). The answer is a durable conversation fact, so the
// resolver is idempotent and also covers a reply made while the daemon was down.
// Runs are coalesced to at most one per minInterval; the timer recovers a missed event.
func startCoordinatorAskResolution(ctx context.Context, recovered <-chan struct{}, events *cdc.Broadcaster, resolver coordinatorAskResolver, minInterval time.Duration, log *slog.Logger) <-chan struct{} {
	done := make(chan struct{})
	wake := make(chan struct{}, 1)
	unsubscribe := events.Subscribe(func(cdc.Event) {
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
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			if err := resolver.ResolveAnsweredCoordinatorAsks(ctx); err != nil && ctx.Err() == nil {
				log.Warn("coordinator question resolution deferred", "err", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			case <-wake:
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(minInterval):
			}
		}
	}()
	return done
}
