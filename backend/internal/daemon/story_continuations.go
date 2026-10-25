package daemon

import (
	"context"
	"log/slog"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/cdc"
	storysvc "github.com/aoagents/agent-orchestrator/backend/internal/service/story"
)

// Existing CDC wakes reconciliation promptly; the timer recovers missed events
// and durable retry deadlines. Start only after provider-host recovery completes.
func startStoryContinuations(ctx context.Context, recovered <-chan struct{}, events *cdc.Broadcaster, runner *storysvc.Continuations, log *slog.Logger) <-chan struct{} {
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
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			if ctx.Err() != nil {
				return
			}
			if err := runner.RunOnce(ctx); err != nil && ctx.Err() == nil {
				log.Warn("story continuation reconciliation deferred", "err", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			case <-wake:
			}
		}
	}()
	return done
}
