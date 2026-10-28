//go:build darwin

package cli

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/mobilebridge"
)

func newWakeHoldEnv(cfg config.Config, lease *mobilebridge.LidAwake) wakeHoldEnv {
	return wakeHoldEnv{
		supported: func() bool {
			supported, _, _ := lease.Status()
			return supported
		},
		lidOpen: func() (open, known bool) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			out, err := exec.CommandContext(ctx, "/usr/sbin/ioreg", "-r", "-k", "AppleClamshellState", "-d", "4").Output()
			if err != nil {
				return false, false
			}
			closed, known := parseClamshellState(string(out))
			return known && !closed, known
		},
		liveLeases: func() []string { return readWakeStatusLeases(filepath.Join(cfg.DataDir, "wake", "status.json")) },
		daemonPID:  func() int { return readRunFilePID(cfg.RunFilePath) },
		pidAlive: func(pid int) bool {
			err := syscall.Kill(pid, 0)
			return err == nil || errors.Is(err, syscall.EPERM)
		},
		now: time.Now,
		wait: func(ctx context.Context, d time.Duration) {
			timer := time.NewTimer(d)
			defer timer.Stop()
			select {
			case <-ctx.Done():
			case <-timer.C:
			}
		},
		maxDuration: wakeHoldMaxDuration,
		poll:        wakeHoldPoll,
	}
}
