package daemon

import (
	"context"
	"log/slog"
	"time"
)

type standingInstructionDeliverer interface {
	DeliverCoordinatorStandingInstructions(context.Context) (bool, error)
}

// startCoordinatorStandingInstructions teaches an existing overall coordinator
// the current human-contact rules in place. It stops as soon as the revision is
// delivered (a durable message, so a restart does not resend it) and only
// retries while a coordinator is missing or a send was uncertain.
func startCoordinatorStandingInstructions(ctx context.Context, recovered <-chan struct{}, deliverer standingInstructionDeliverer, retry time.Duration, log *slog.Logger) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-ctx.Done():
			return
		case <-recovered:
		}
		timer := time.NewTimer(0)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			delivered, err := deliverer.DeliverCoordinatorStandingInstructions(ctx)
			if err != nil && ctx.Err() == nil {
				log.Warn("coordinator standing instructions deferred", "err", err)
			}
			if delivered && err == nil {
				return
			}
			timer.Reset(retry)
		}
	}()
	return done
}
