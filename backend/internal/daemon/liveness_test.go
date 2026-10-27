package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

type fakeLivenessStore struct {
	mu        sync.Mutex
	calls     []string
	heartbeat int
	startErr  error
	prev      *domain.DaemonRun
	prevErr   error
	sessions  []domain.SessionRecord
	listErr   error
}

func (f *fakeLivenessStore) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}
func (f *fakeLivenessStore) StartDaemonRun(context.Context, string, time.Time) error {
	f.record("start")
	return f.startErr
}
func (f *fakeLivenessStore) HeartbeatDaemonRun(context.Context, string, time.Time) error {
	f.mu.Lock()
	f.heartbeat++
	f.mu.Unlock()
	return nil
}
func (f *fakeLivenessStore) MarkDaemonShutdownStarted(context.Context, string, time.Time) error {
	f.record("shutdown")
	return nil
}
func (f *fakeLivenessStore) MarkDaemonEndedClean(context.Context, string, time.Time) error {
	f.record("clean")
	return nil
}
func (f *fakeLivenessStore) PreviousDaemonRun(context.Context, string) (domain.DaemonRun, bool, error) {
	if f.prevErr != nil {
		return domain.DaemonRun{}, false, f.prevErr
	}
	if f.prev == nil {
		return domain.DaemonRun{}, false, nil
	}
	return *f.prev, true, nil
}
func (f *fakeLivenessStore) ListAllSessions(context.Context) ([]domain.SessionRecord, error) {
	return f.sessions, f.listErr
}
func (f *fakeLivenessStore) snapshot() ([]string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...), f.heartbeat
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestLivenessStampsStartHeartbeatShutdownAndCleanEndInOrder(t *testing.T) {
	store := &fakeLivenessStore{}
	l := newDaemonLiveness(store, "boot", quietLog())
	l.interval = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	l.watchShutdown(ctx)

	l.start(ctx)
	eventually(t, "heartbeats", func() bool { _, beats := store.snapshot(); return beats >= 3 })

	cancel()
	l.wait()
	eventually(t, "the shutdown stamp", func() bool { calls, _ := store.snapshot(); return len(calls) >= 2 })
	l.endedClean()

	calls, _ := store.snapshot()
	want := []string{"start", "shutdown", "clean"}
	if len(calls) != len(want) {
		t.Fatalf("stamp order = %v, want %v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("stamp order = %v, want %v", calls, want)
		}
	}
	_, before := store.snapshot()
	time.Sleep(40 * time.Millisecond)
	if _, after := store.snapshot(); after != before {
		t.Fatalf("the heartbeat kept running after shutdown: %d -> %d", before, after)
	}
}

func TestLivenessDoesNotStartAfterShutdownBegan(t *testing.T) {
	store := &fakeLivenessStore{}
	l := newDaemonLiveness(store, "boot", quietLog())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	l.start(ctx)
	l.wait()
	if calls, beats := store.snapshot(); len(calls) != 0 || beats != 0 {
		t.Fatalf("a run was recorded after shutdown began: %v %d", calls, beats)
	}

	// A start that loses the race with wait() must also do nothing.
	l2 := newDaemonLiveness(store, "boot2", quietLog())
	l2.wait()
	l2.start(context.Background())
	if calls, _ := store.snapshot(); len(calls) != 0 {
		t.Fatalf("a run was recorded after wait(): %v", calls)
	}
}

func TestLivenessSurvivesAStoreThatCannotRecordTheRun(t *testing.T) {
	store := &fakeLivenessStore{startErr: errors.New("disk full")}
	l := newDaemonLiveness(store, "boot", quietLog())
	l.interval = 2 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l.start(ctx)
	l.wait() // must not hang: no heartbeat loop was started
	if _, beats := store.snapshot(); beats != 0 {
		t.Fatalf("heartbeats ran for a run that was never recorded: %d", beats)
	}
}

func TestReadBootWindowUsesThePreviousRunAndFallsBackOnce(t *testing.T) {
	boot := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	prev := &domain.DaemonRun{StartedAt: boot.Add(-time.Hour), HeartbeatAt: boot.Add(-5 * time.Minute)}
	w := readBootWindow(context.Background(), &fakeLivenessStore{prev: prev}, "boot", boot, quietLog())
	if !w.HasPrevious || !w.PrevEnd.Equal(prev.HeartbeatAt) {
		t.Fatalf("window = %+v", w)
	}

	sessions := []domain.SessionRecord{{UpdatedAt: boot.Add(-4 * time.Minute)}, {UpdatedAt: boot.Add(-2 * time.Minute)}, {UpdatedAt: boot.Add(-9 * time.Hour)}}
	fallback := readBootWindow(context.Background(), &fakeLivenessStore{sessions: sessions}, "boot", boot, quietLog())
	if !fallback.Fallback || !fallback.PrevEnd.Equal(boot.Add(-2*time.Minute)) {
		t.Fatalf("first-boot fallback must anchor on the newest session update: %+v", fallback)
	}
}

func TestReadBootWindowFailsSafeWhenTheStoreCannotBeRead(t *testing.T) {
	boot := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	w := readBootWindow(context.Background(), &fakeLivenessStore{prevErr: errors.New("locked")}, "boot", boot, quietLog())
	if w.RuleBEnabled() {
		t.Fatalf("an unreadable previous run must switch the shutdown-window rule off: %+v", w)
	}
	w = readBootWindow(context.Background(), &fakeLivenessStore{listErr: errors.New("locked")}, "boot", boot, quietLog())
	if w.RuleBEnabled() {
		t.Fatalf("unreadable sessions must switch the first-boot fallback off: %+v", w)
	}
}
