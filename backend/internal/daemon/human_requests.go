package daemon

import (
	"context"
	"time"
)

type humanRequestSweeper interface{ Sweep(ctx context.Context) }

// startHumanRequestSweeper expires overdue human requests and tells their requesters even when
// the app is closed and nobody is listing them. It returns when ctx is done.
func startHumanRequestSweeper(ctx context.Context, sweeper humanRequestSweeper, every time.Duration) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				sweeper.Sweep(ctx)
			}
		}
	}()
	return done
}
