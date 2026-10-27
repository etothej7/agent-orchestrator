package daemon

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// livenessHeartbeat is how often a running daemon refreshes its liveness stamp. It
// bounds how far the stamp can lag a kill -9 or a power loss.
const livenessHeartbeat = 10 * time.Second

// livenessStampTimeout bounds one stamp write. The shutdown stamps run while the
// daemon is going away, so they must never hold it up.
const livenessStampTimeout = 2 * time.Second

// livenessStore is the slice of the store the liveness stamps use.
type livenessStore interface {
	StartDaemonRun(ctx context.Context, bootID string, now time.Time) error
	HeartbeatDaemonRun(ctx context.Context, bootID string, now time.Time) error
	MarkDaemonShutdownStarted(ctx context.Context, bootID string, now time.Time) error
	MarkDaemonEndedClean(ctx context.Context, bootID string, now time.Time) error
}

// bootWindowStore is what reading the previous run needs.
type bootWindowStore interface {
	PreviousDaemonRun(ctx context.Context, bootID string) (domain.DaemonRun, bool, error)
	ListAllSessions(ctx context.Context) ([]domain.SessionRecord, error)
}

// readBootWindow reads, ONCE and before any health pass or startup write, what the
// previous daemon run left behind. A read failure yields an empty window (no
// shutdown-window capture) rather than a guess: auto-resume then falls back to the
// rule that does not need it.
func readBootWindow(ctx context.Context, store bootWindowStore, bootID string, now time.Time, log *slog.Logger) domain.BootWindow {
	var prev *domain.DaemonRun
	run, found, err := store.PreviousDaemonRun(ctx, bootID)
	switch {
	case err != nil:
		log.Warn("auto-resume: previous daemon run unreadable; shutdown-window capture is off for this boot", "err", err)
		return domain.NewBootWindow(bootID, now, nil, time.Time{})
	case found:
		prev = &run
	}
	var newest time.Time
	if prev == nil {
		sessions, listErr := store.ListAllSessions(ctx)
		if listErr != nil {
			log.Warn("auto-resume: sessions unreadable for the first-boot fallback; shutdown-window capture is off for this boot", "err", listErr)
		}
		for _, rec := range sessions {
			if rec.UpdatedAt.After(newest) {
				newest = rec.UpdatedAt
			}
		}
	}
	return domain.NewBootWindow(bootID, now, prev, newest)
}

// daemonLiveness stamps this daemon run so the NEXT boot can tell which exits
// happened during this run's shutdown and how often the daemon has been restarting.
type daemonLiveness struct {
	store    livenessStore
	bootID   string
	log      *slog.Logger
	now      func() time.Time
	interval time.Duration

	mu      sync.Mutex
	closing bool
	wg      sync.WaitGroup
}

func newDaemonLiveness(store livenessStore, bootID string, log *slog.Logger) *daemonLiveness {
	return &daemonLiveness{store: store, bootID: bootID, log: log, now: func() time.Time { return time.Now().UTC() }, interval: livenessHeartbeat}
}

// start records this run and keeps its heartbeat fresh until ctx ends. It is called
// AFTER the boot health pass, so a boot that dies during the pass leaves no row that
// could mask the previous run's shutdown window. After shutdown has begun it does
// nothing.
func (l *daemonLiveness) start(ctx context.Context) {
	l.mu.Lock()
	if l.closing || ctx.Err() != nil {
		l.mu.Unlock()
		return
	}
	l.wg.Add(1)
	l.mu.Unlock()

	if err := l.store.StartDaemonRun(ctx, l.bootID, l.now()); err != nil {
		l.log.Warn("auto-resume: could not record this daemon run; the next boot cannot use its shutdown window", "err", err)
		l.wg.Done()
		return
	}
	go func() {
		defer l.wg.Done()
		ticker := time.NewTicker(l.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				stampCtx, cancel := context.WithTimeout(ctx, livenessStampTimeout)
				if err := l.store.HeartbeatDaemonRun(stampCtx, l.bootID, l.now()); err != nil && ctx.Err() == nil {
					l.log.Warn("auto-resume: daemon heartbeat failed", "err", err)
				}
				cancel()
			}
		}
	}()
}

// watchShutdown stamps the START of a graceful shutdown the moment the signal
// context cancels, before any controller or host is touched.
func (l *daemonLiveness) watchShutdown(ctx context.Context) {
	go func() {
		<-ctx.Done()
		l.stamp("shutdown start", l.store.MarkDaemonShutdownStarted)
	}()
}

// wait stops accepting a late start and waits for the heartbeat loop to end. Call it
// after the context was cancelled.
func (l *daemonLiveness) wait() {
	l.mu.Lock()
	l.closing = true
	l.mu.Unlock()
	l.wg.Wait()
}

// endedClean stamps the end of a graceful shutdown.
func (l *daemonLiveness) endedClean() {
	l.stamp("clean end", l.store.MarkDaemonEndedClean)
}

func (l *daemonLiveness) stamp(what string, write func(context.Context, string, time.Time) error) {
	ctx, cancel := context.WithTimeout(context.Background(), livenessStampTimeout)
	defer cancel()
	if err := write(ctx, l.bootID, l.now()); err != nil {
		l.log.Warn("auto-resume: could not stamp the daemon "+what, "err", err)
	}
}
