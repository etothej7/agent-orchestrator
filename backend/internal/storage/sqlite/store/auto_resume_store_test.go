package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

func autoResumeSession(t *testing.T) (*sqlite.Store, domain.SessionID) {
	db, id, _ := autoResumeSessionAt(t)
	return db, id
}

func autoResumeSessionAt(t *testing.T) (*sqlite.Store, domain.SessionID, string) {
	t.Helper()
	dir := t.TempDir()
	db, err := sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	seedProject(t, db, "repo")
	rec, err := db.CreateSession(context.Background(), sampleRecord("repo"))
	if err != nil {
		t.Fatal(err)
	}
	return db, rec.ID, dir
}

// autoResumeRows counts the rows in session_auto_resume through a separate read-only handle.
func autoResumeRows(t *testing.T, dir string) int {
	t.Helper()
	raw, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "ao.db")+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	var n int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM session_auto_resume`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAutoResumePolicyDefaultsToAllowedAndFailsClosed(t *testing.T) {
	db, id := autoResumeSession(t)
	ctx := context.Background()
	now := time.Now().UTC()

	policy, err := db.AutoResumePolicy(ctx, id)
	if err != nil || policy != domain.AutoResumeAllowed || !db.AutoResumeAllowed(ctx, id) {
		t.Fatalf("a session with no stored policy must be allowed: policy=%q err=%v", policy, err)
	}
	if err := db.SetAutoResumePolicy(ctx, id, domain.AutoResumeNever, now); err != nil {
		t.Fatal(err)
	}
	if db.AutoResumeAllowed(ctx, id) {
		t.Fatal("a never session must not be allowed")
	}
	if err := db.SetAutoResumePolicy(ctx, id, domain.AutoResumeAllowed, now); err != nil {
		t.Fatal(err)
	}
	if !db.AutoResumeAllowed(ctx, id) {
		t.Fatal("lifting never must allow the session again")
	}
	if err := db.SetAutoResumePolicy(ctx, id, "sometimes", now); err == nil {
		t.Fatal("an unknown policy value was accepted")
	}

	// Fail closed: a policy that cannot be read is never an allowance.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if db.AutoResumeAllowed(cancelled, id) {
		t.Fatal("an unreadable policy was treated as allowed")
	}
	if got, err := db.AutoResumePolicy(cancelled, id); err == nil || got.Allows() {
		t.Fatalf("an unreadable policy must report an error and not allow: policy=%q err=%v", got, err)
	}
	// A policy for a session that does not exist cannot be stored.
	if err := db.SetAutoResumePolicy(ctx, "missing-1", domain.AutoResumeNever, now); err == nil {
		t.Fatal("a policy was stored for a session that does not exist")
	}
}

func TestStopReasonIsRecordedClearedAndKeepsThePolicy(t *testing.T) {
	db, id, dir := autoResumeSessionAt(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if reason, err := db.StopReason(ctx, id); err != nil || reason != domain.StopReasonNone {
		t.Fatalf("default stop reason = %q err=%v", reason, err)
	}
	if err := db.SetAutoResumePolicy(ctx, id, domain.AutoResumeNever, now); err != nil {
		t.Fatal(err)
	}
	if err := db.SetStopReason(ctx, id, domain.StopReasonUser, now); err != nil {
		t.Fatal(err)
	}
	if reason, _ := db.StopReason(ctx, id); reason != domain.StopReasonUser {
		t.Fatalf("stop reason = %q", reason)
	}
	if db.AutoResumeAllowed(ctx, id) {
		t.Fatal("recording a stop reason replaced the never policy")
	}
	if err := db.SetStopReason(ctx, id, domain.StopReasonNone, now); err != nil {
		t.Fatal(err)
	}
	if reason, _ := db.StopReason(ctx, id); reason != domain.StopReasonNone {
		t.Fatalf("stop reason not cleared: %q", reason)
	}
	if err := db.SetStopReason(ctx, id, "because", now); err == nil {
		t.Fatal("an unknown stop reason was accepted")
	}
	// Clearing a reason on a session that has none writes nothing, so every controller
	// start (which clears) cannot grow a row per session.
	clean, err := db.CreateSession(ctx, sampleRecord("repo"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetStopReason(ctx, clean.ID, domain.StopReasonNone, now); err != nil {
		t.Fatal(err)
	}
	if n := autoResumeRows(t, dir); n != 1 {
		t.Fatalf("clearing created a row: %d rows for two sessions, want only the one that had a policy", n)
	}
	// Recording a stop reason first must not invent a never policy.
	other, err := db.CreateSession(ctx, sampleRecord("repo"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetStopReason(ctx, other.ID, domain.StopReasonAccount, now); err != nil {
		t.Fatal(err)
	}
	if !db.AutoResumeAllowed(ctx, other.ID) {
		t.Fatal("a stop reason on a session with no policy made it protected")
	}
}

func TestDaemonRunsKeepALivenessAndBootLog(t *testing.T) {
	db, _ := autoResumeSession(t)
	ctx := context.Background()
	base := time.Date(2026, 10, 27, 8, 0, 0, 0, time.UTC)

	if _, found, err := db.PreviousDaemonRun(ctx, "boot-0"); err != nil || found {
		t.Fatalf("no previous run expected: found=%v err=%v", found, err)
	}
	for i := 0; i < 25; i++ {
		if err := db.StartDaemonRun(ctx, fmt.Sprintf("boot-%02d", i), base.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	since, err := db.DaemonRunsSince(ctx, time.Time{})
	if err != nil || since != 20 {
		t.Fatalf("the boot log must keep the last 20 runs: got %d err=%v", since, err)
	}

	if err := db.StartDaemonRun(ctx, "current", base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	prev, found, err := db.PreviousDaemonRun(ctx, "current")
	if err != nil || !found || prev.BootID == "current" {
		t.Fatalf("the previous run must exclude the current boot: %+v found=%v err=%v", prev, found, err)
	}

	if err := db.HeartbeatDaemonRun(ctx, "current", base.Add(time.Hour+10*time.Second)); err != nil {
		t.Fatal(err)
	}
	first := base.Add(time.Hour + 20*time.Second)
	if err := db.MarkDaemonShutdownStarted(ctx, "current", first); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkDaemonShutdownStarted(ctx, "current", first.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkDaemonEndedClean(ctx, "current", first.Add(8*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := db.StartDaemonRun(ctx, "next", base.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	run, found, err := db.PreviousDaemonRun(ctx, "next")
	if err != nil || !found || run.BootID != "current" {
		t.Fatalf("previous run = %+v found=%v err=%v", run, found, err)
	}
	if run.ShutdownStartedAt == nil || !run.ShutdownStartedAt.Equal(first) {
		t.Fatalf("the first shutdown stamp must win: %v", run.ShutdownStartedAt)
	}
	if run.EndedCleanAt == nil || !run.End().Equal(first.Add(8*time.Second)) {
		t.Fatalf("run end = %v, want the clean end", run.End())
	}
	recent, err := db.DaemonRunsSince(ctx, base.Add(time.Hour))
	if err != nil || recent != 2 {
		t.Fatalf("runs since the hour = %d err=%v", recent, err)
	}
}

func TestDaemonRunEndIsTheLatestLivenessFact(t *testing.T) {
	start := time.Date(2026, 10, 27, 8, 0, 0, 0, time.UTC)
	hb := start.Add(30 * time.Second)
	shutdown := start.Add(40 * time.Second)
	clean := start.Add(45 * time.Second)
	cases := []struct {
		name string
		run  domain.DaemonRun
		want time.Time
	}{
		{"only a start", domain.DaemonRun{StartedAt: start, HeartbeatAt: start}, start},
		{"heartbeat", domain.DaemonRun{StartedAt: start, HeartbeatAt: hb}, hb},
		{"shutdown after heartbeat", domain.DaemonRun{StartedAt: start, HeartbeatAt: hb, ShutdownStartedAt: &shutdown}, shutdown},
		{"clean end", domain.DaemonRun{StartedAt: start, HeartbeatAt: hb, ShutdownStartedAt: &shutdown, EndedCleanAt: &clean}, clean},
	}
	for _, c := range cases {
		if got := c.run.End(); !got.Equal(c.want) {
			t.Errorf("%s: End() = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestResumeIntentCaptureKeepsALiveIntentAndCarriesRecentResumes(t *testing.T) {
	db, id := autoResumeSession(t)
	ctx := context.Background()
	t0 := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	fresh := func(boot string, at time.Time) domain.ResumeIntent {
		return domain.ResumeIntent{
			SessionID: id, BootID: boot, CapturedAt: at, Class: domain.ResumeWorking,
			PriorActivity: domain.ActivityActive, State: domain.ResumePending, UpdatedAt: at,
			InterruptedTurns: []domain.InterruptedTurn{{ID: "turn-1", Delivered: true}},
		}
	}

	first, created, err := db.CaptureResumeIntent(ctx, fresh("boot-1", t0))
	if err != nil || !created || first.Revision != 1 {
		t.Fatalf("first capture: %+v created=%v err=%v", first, created, err)
	}

	// The resumer makes progress, then a second boot captures again before it finished.
	first.Attempts = 3
	first.State = domain.ResumeResuming
	if ok, err := db.SaveResumeIntent(ctx, first); err != nil || !ok {
		t.Fatalf("save: ok=%v err=%v", ok, err)
	}
	kept, created, err := db.CaptureResumeIntent(ctx, fresh("boot-2", t0.Add(time.Minute)))
	if err != nil || created {
		t.Fatalf("a live intent must be kept: created=%v err=%v", created, err)
	}
	if kept.BootID != "boot-1" || kept.Attempts != 3 || kept.State != domain.ResumeResuming {
		t.Fatalf("a second boot reset a live intent: %+v", kept)
	}

	// A finished intent is replaced, keeping the recent-resume history for the flap guard.
	kept.State = domain.ResumeResumed
	kept.RecentResumes = []time.Time{t0, t0.Add(10 * time.Minute)}
	if ok, err := db.SaveResumeIntent(ctx, kept); err != nil || !ok {
		t.Fatalf("save finished: ok=%v err=%v", ok, err)
	}
	next, created, err := db.CaptureResumeIntent(ctx, fresh("boot-3", t0.Add(time.Hour)))
	if err != nil || !created || next.BootID != "boot-3" || next.Attempts != 0 || next.State != domain.ResumePending {
		t.Fatalf("a finished intent must be replaced by a fresh one: %+v created=%v err=%v", next, created, err)
	}
	if len(next.RecentResumes) != 2 {
		t.Fatalf("recent resumes were not carried over: %v", next.RecentResumes)
	}
	read, found, err := db.GetResumeIntent(ctx, id)
	if err != nil || !found || read.BootID != "boot-3" || len(read.RecentResumes) != 2 || len(read.InterruptedTurns) != 1 || !read.InterruptedTurns[0].Delivered {
		t.Fatalf("stored intent = %+v found=%v err=%v", read, found, err)
	}
}

func TestResumeIntentSaveIsCompareAndSwapListDeleteAndCascade(t *testing.T) {
	db, id := autoResumeSession(t)
	ctx := context.Background()
	at := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	intent, _, err := db.CaptureResumeIntent(ctx, domain.ResumeIntent{SessionID: id, BootID: "b", CapturedAt: at, Class: domain.ResumeIdle, State: domain.ResumePending, UpdatedAt: at})
	if err != nil {
		t.Fatal(err)
	}
	stale := intent
	intent.State = domain.ResumeHibernated
	if ok, err := db.SaveResumeIntent(ctx, intent); err != nil || !ok {
		t.Fatalf("save: ok=%v err=%v", ok, err)
	}
	stale.State = domain.ResumeFailed
	if ok, err := db.SaveResumeIntent(ctx, stale); err != nil || ok {
		t.Fatalf("a stale writer won the compare-and-swap: ok=%v err=%v", ok, err)
	}
	all, err := db.ListResumeIntents(ctx)
	if err != nil || len(all) != 1 || all[0].State != domain.ResumeHibernated || all[0].Revision != 2 {
		t.Fatalf("list = %+v err=%v", all, err)
	}
	if err := db.DeleteResumeIntent(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := db.GetResumeIntent(ctx, id); found {
		t.Fatal("intent not deleted")
	}
	if _, _, err := db.CaptureResumeIntent(ctx, domain.ResumeIntent{SessionID: "missing-1", BootID: "b", State: domain.ResumePending}); err == nil {
		t.Fatal("an intent was captured for a session that does not exist")
	}
}
