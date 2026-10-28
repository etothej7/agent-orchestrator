package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/mobilebridge"
)

const (
	wakeHoldPoll = 2 * time.Second
	// wakeHoldMaxDuration bounds a hold nobody ends: a closed-lid Mac left awake for good would
	// run hot in a bag. The wake helper's own battery and thermal stops apply to this lease too.
	wakeHoldMaxDuration = 12 * time.Hour
)

// Why a hold ended.
const (
	wakeHoldUnsupported = "unsupported"
	wakeHoldNoLease     = "no lease"
	wakeHoldLidOpen     = "lid open"
	wakeHoldAOBack      = "ao back"
	wakeHoldExpired     = "expired"
	wakeHoldCancelled   = "cancelled"
)

// wakeHoldLease is the part of mobilebridge.LidAwake a hold needs.
type wakeHoldLease interface {
	Start(allowSetup bool) error
	Stop()
	LeaseID() string
}

// wakeHoldEnv is everything a hold reads from the machine, so the loop is testable.
type wakeHoldEnv struct {
	supported func() bool
	// lidOpen reports whether the lid is open and whether that is known at all.
	lidOpen func() (open, known bool)
	// liveLeases lists the lease ids the wake helper currently accepts.
	liveLeases func() []string
	// daemonPID is the pid in AO's run file, 0 when there is none.
	daemonPID   func() int
	pidAlive    func(pid int) bool
	now         func() time.Time
	wait        func(ctx context.Context, d time.Duration)
	maxDuration time.Duration
	poll        time.Duration
}

// runWakeHold keeps the wake helper's closed-lid override armed after AO quits.
//
// Quitting withdraws AO's own lease, and with the lid shut macOS then puts the Mac into
// clamshell sleep at once: the display goes dark and the lock screen follows. A caffeinate
// assertion does not prevent that; only the helper's override does, and only while some
// live lease asks for it. So a detached process owns a lease of its own until AO is back.
//
// It ends when AO comes back (a lease or daemon that was not there when the hold began), the lid
// is opened, the cap passes, or it is signalled. It returns why.
func runWakeHold(ctx context.Context, lease wakeHoldLease, env wakeHoldEnv) (string, error) {
	if !env.supported() {
		return wakeHoldUnsupported, nil
	}
	if open, known := env.lidOpen(); known && open {
		return wakeHoldLidOpen, nil // nothing can sleep because of a quit with the lid up
	}
	initialPID := env.daemonPID()
	before := map[string]bool{}
	for _, id := range env.liveLeases() {
		before[id] = true
	}
	if err := lease.Start(false); err != nil {
		return "", err
	}
	defer lease.Stop()
	own := lease.LeaseID()
	if own == "" {
		return wakeHoldNoLease, nil // the helper is not set up: Start wrote nothing to withdraw
	}
	deadline := env.now().Add(env.maxDuration)
	for {
		if ctx.Err() != nil {
			return wakeHoldCancelled, nil
		}
		env.wait(ctx, env.poll)
		if ctx.Err() != nil {
			return wakeHoldCancelled, nil
		}
		if !env.now().Before(deadline) {
			return wakeHoldExpired, nil
		}
		if open, known := env.lidOpen(); known && open {
			return wakeHoldLidOpen, nil
		}
		for _, id := range env.liveLeases() {
			if id != own && !before[id] {
				return wakeHoldAOBack, nil
			}
		}
		if pid := env.daemonPID(); pid != 0 && pid != initialPID && env.pidAlive(pid) {
			return wakeHoldAOBack, nil
		}
	}
}

// parseClamshellState reads the lid from `ioreg -r -k AppleClamshellState -d 4`. closed is
// meaningful only when known; a Mac with no lid, or unexpected output, is not known.
func parseClamshellState(out string) (closed, known bool) {
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "|")), "=")
		if !ok || strings.TrimSpace(key) != `"AppleClamshellState"` {
			continue
		}
		switch strings.TrimSpace(value) {
		case "Yes":
			return true, true
		case "No":
			return false, true
		}
	}
	return false, false
}

// readWakeStatusLeases lists the lease ids the wake helper currently accepts, from its status file
// (<data dir>/wake/status.json, written by frontend/native/wake-helper). Unreadable means none.
func readWakeStatusLeases(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var status struct {
		Leases []string `json:"leases"`
	}
	if json.Unmarshal(data, &status) != nil {
		return nil
	}
	return status.Leases
}

// readRunFilePID is the daemon pid in AO's run file (running.json), 0 when there is none.
func readRunFilePID(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	var run struct {
		PID int `json:"pid"`
	}
	if json.Unmarshal(data, &run) != nil {
		return 0
	}
	return run.PID
}

func newWakeHoldCommand() *cobra.Command {
	maxDuration := wakeHoldMaxDuration
	cmd := &cobra.Command{
		Use:    "wake-hold",
		Short:  "Keep a closed-lid Mac awake after AO quits, until AO is reopened (internal)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("wake-hold: %w", err)
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGTERM, os.Interrupt)
			defer stop()
			lease := mobilebridge.NewLidAwake(os.Getpid(), os.Getenv("AO_WAKE_HELPER"), cfg.DataDir)
			env := newWakeHoldEnv(cfg, lease)
			env.maxDuration = maxDuration
			_, err = runWakeHold(ctx, lease, env)
			return err
		},
	}
	cmd.Flags().DurationVar(&maxDuration, "max-duration", wakeHoldMaxDuration, "Longest the hold may last if AO is never reopened")
	return cmd
}
