package domain

import (
	"testing"
	"time"
)

func TestAutoResumePolicyFailsClosed(t *testing.T) {
	for policy, want := range map[AutoResumePolicy]bool{AutoResumeAllowed: true, AutoResumeNever: false, "": false, "ALLOWED": false, "garbage": false} {
		if got := policy.Allows(); got != want {
			t.Errorf("%q.Allows() = %v, want %v", policy, got, want)
		}
	}
	if !AutoResumeAllowed.Valid() || !AutoResumeNever.Valid() || AutoResumePolicy("").Valid() {
		t.Error("only allowed and never are storable policies")
	}
}

func TestResumeStateLive(t *testing.T) {
	for state, want := range map[ResumeState]bool{ResumePending: true, ResumeResuming: true, ResumeResumed: false, ResumeHibernated: false, ResumeFailed: false, ResumeAbandoned: false} {
		if got := state.Live(); got != want {
			t.Errorf("%s.Live() = %v, want %v", state, got, want)
		}
	}
}

func TestBootWindowShutdownWindow(t *testing.T) {
	boot := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	prevStart := boot.Add(-3 * time.Hour)
	heartbeat := boot.Add(-5 * time.Minute)
	shutdown := heartbeat.Add(4 * time.Second)
	prev := &DaemonRun{StartedAt: prevStart, HeartbeatAt: heartbeat, ShutdownStartedAt: &shutdown}
	w := NewBootWindow("boot", boot, prev, time.Time{})

	if !w.RuleBEnabled() || !w.PrevEnd.Equal(shutdown) {
		t.Fatalf("window = %+v", w)
	}
	cases := []struct {
		name string
		at   time.Time
		want bool
	}{
		{"exactly at the last stamp", shutdown, true},
		{"a minute before it", shutdown.Add(-time.Minute), true},
		{"just over a minute before it", shutdown.Add(-time.Minute - time.Second), false},
		{"three days ago", boot.Add(-72 * time.Hour), false},
		// Hosts-first graceful order: the dying daemon records the exit AFTER its last
		// heartbeat and after its shutdown stamp.
		{"strictly after the last heartbeat and the shutdown stamp", shutdown.Add(9 * time.Second), true},
		{"right before this boot", boot.Add(-time.Second), true},
		{"at this boot", boot, true},
		{"after this boot started", boot.Add(time.Second), false},
	}
	for _, c := range cases {
		if got := w.InShutdownWindow(c.at); got != c.want {
			t.Errorf("%s: InShutdownWindow = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestBootWindowStartsNoEarlierThanThePreviousRun(t *testing.T) {
	boot := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	// A previous boot that lived 20 s: its own health pass wrote exits at its start.
	prevStart := boot.Add(-30 * time.Second)
	prev := &DaemonRun{StartedAt: prevStart, HeartbeatAt: prevStart.Add(20 * time.Second)}
	w := NewBootWindow("boot", boot, prev, time.Time{})
	if !w.ShutdownWindowStart().Equal(prevStart) {
		t.Fatalf("window start = %v, want the previous run's start %v", w.ShutdownWindowStart(), prevStart)
	}
	if w.InShutdownWindow(prevStart.Add(-time.Second)) {
		t.Fatal("an exit from before the previous run started was attributed to its shutdown")
	}
	if !w.InShutdownWindow(prevStart.Add(25 * time.Second)) {
		t.Fatal("an exit during the previous run's shutdown was missed")
	}
}

func TestBootWindowClampsAClockThatJumpedBack(t *testing.T) {
	boot := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	prev := &DaemonRun{StartedAt: boot.Add(time.Hour), HeartbeatAt: boot.Add(2 * time.Hour)}
	w := NewBootWindow("boot", boot, prev, time.Time{})
	if w.PrevEnd.After(boot) || w.PrevStart.After(boot) {
		t.Fatalf("a previous run from the future was not clamped: %+v", w)
	}
}

func TestBootWindowFirstBootFallback(t *testing.T) {
	boot := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)

	fresh := NewBootWindow("boot", boot, nil, boot.Add(-3*time.Minute))
	if !fresh.Fallback || fresh.HasPrevious || !fresh.RuleBEnabled() {
		t.Fatalf("a fresh fallback must enable rule B: %+v", fresh)
	}
	if !fresh.InShutdownWindow(boot.Add(-3*time.Minute)) || fresh.InShutdownWindow(boot.Add(-5*time.Minute)) {
		t.Fatal("the fallback window must be anchored on the newest session update")
	}

	stale := NewBootWindow("boot", boot, nil, boot.Add(-FirstBootFallbackMaxAge-time.Second))
	if stale.RuleBEnabled() || stale.InShutdownWindow(stale.PrevEnd) {
		t.Fatalf("a stale fallback must disable rule B: %+v", stale)
	}

	none := NewBootWindow("boot", boot, nil, time.Time{})
	if none.RuleBEnabled() || none.InShutdownWindow(boot.Add(-time.Second)) {
		t.Fatalf("no previous run and no sessions means no capture: %+v", none)
	}
}
