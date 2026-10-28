//go:build !darwin

package cli

import (
	"context"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/mobilebridge"
)

// Closed-lid wake exists only on macOS; elsewhere a hold has nothing to do.
func newWakeHoldEnv(config.Config, *mobilebridge.LidAwake) wakeHoldEnv {
	return wakeHoldEnv{
		supported:  func() bool { return false },
		lidOpen:    func() (bool, bool) { return false, false },
		liveLeases: func() []string { return nil },
		daemonPID:  func() int { return 0 },
		pidAlive:   func(int) bool { return false },
		now:        time.Now,
		wait:       func(context.Context, time.Duration) {},
	}
}
