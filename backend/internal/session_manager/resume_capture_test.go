package sessionmanager

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

var captureBoot = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

// captureWindow is a boot whose previous run was alive until 11:59:50 (a heartbeat
// plus its shutdown), so Rule B covers exits from 11:58:50 up to this boot.
func captureWindow() domain.BootWindow {
	prev := domain.DaemonRun{BootID: "prev", StartedAt: captureBoot.Add(-6 * time.Hour), HeartbeatAt: captureBoot.Add(-10 * time.Second)}
	return domain.NewBootWindow("boot-2", captureBoot, &prev, time.Time{})
}

type captureFixture struct {
	m   *Manager
	st  *fakeStore
	rec domain.SessionRecord
}

// newCaptureFixture seeds a chat session whose provider host is gone, the state a
// reboot leaves it in, and runs nothing yet.
func newCaptureFixture(t *testing.T, state domain.ActivityState, lastActivity time.Time) *captureFixture {
	t.Helper()
	launcher := &recordingLauncher{startErr: ports.ErrChatHostNotRunning}
	m, st, _ := newChatManager(launcher)
	m.SetBootWindow(captureWindow())
	m.clock = func() time.Time { return captureBoot }
	seedChatResumeSession(st, state)
	rec := st.sessions["mer-1"]
	rec.Activity.LastActivityAt = lastActivity
	st.sessions["mer-1"] = rec
	return &captureFixture{m: m, st: st, rec: rec}
}

func (f *captureFixture) run(t *testing.T) {
	t.Helper()
	if err := f.m.checkSessionHealth(ctx, f.rec); err != nil {
		t.Fatalf("checkSessionHealth: %v", err)
	}
}

func (f *captureFixture) intent() (domain.ResumeIntent, bool) {
	in, ok := f.st.intents["mer-1"]
	return in, ok
}

func TestCaptureRuleAKeepsAWorkingSessionAndRecordsTheCutOffTurns(t *testing.T) {
	for _, state := range []domain.ActivityState{domain.ActivityActive, domain.ActivityWaitingInput, domain.ActivityBlocked} {
		t.Run(string(state), func(t *testing.T) {
			f := newCaptureFixture(t, state, captureBoot.Add(-time.Hour))
			f.st.interrupted = map[domain.SessionID][]domain.InterruptedTurn{"mer-1": {{ID: "turn-1", Delivered: true}}}
			f.run(t)

			in, ok := f.intent()
			if !ok || in.Class != domain.ResumeWorking || in.State != domain.ResumePending || in.PriorActivity != state ||
				in.BootID != "boot-2" || len(in.InterruptedTurns) != 1 || !in.InterruptedTurns[0].Delivered {
				t.Fatalf("intent = %+v ok=%v, want a pending working intent with the delivered turn", in, ok)
			}
			// Capture never changes what boot does to the row: it still ends up exited.
			if got := f.st.sessions["mer-1"].Activity.State; got != domain.ActivityExited {
				t.Fatalf("boot left the row %q, want exited exactly as before", got)
			}
		})
	}
}

func TestCaptureRuleAIdleSessionIsIdleClass(t *testing.T) {
	f := newCaptureFixture(t, domain.ActivityIdle, captureBoot.Add(-time.Hour))
	f.run(t)
	if in, ok := f.intent(); !ok || in.Class != domain.ResumeIdle {
		t.Fatalf("intent = %+v ok=%v, want an idle intent", in, ok)
	}
}

func TestCaptureTurnsMakeAnIdleSessionWorking(t *testing.T) {
	f := newCaptureFixture(t, domain.ActivityIdle, captureBoot.Add(-time.Hour))
	f.st.interrupted = map[domain.SessionID][]domain.InterruptedTurn{"mer-1": {{ID: "turn-1"}}}
	f.run(t)
	if in, ok := f.intent(); !ok || in.Class != domain.ResumeWorking {
		t.Fatalf("intent = %+v ok=%v, want working (a human turn was cut off)", in, ok)
	}
}

func TestCaptureRuleBOnlyTakesExitsFromThePreviousShutdownWindow(t *testing.T) {
	cases := []struct {
		name     string
		exitedAt time.Time
		want     bool
	}{
		{"exit during the previous daemon's shutdown", captureBoot.Add(-5 * time.Second), true},
		{"exit just inside the 60 s lookback", captureBoot.Add(-70 * time.Second), true},
		{"exit three days ago", captureBoot.Add(-72 * time.Hour), false},
		{"exit before the lookback", captureBoot.Add(-2 * time.Hour), false},
		{"exit after this boot began", captureBoot.Add(time.Second), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newCaptureFixture(t, domain.ActivityExited, tc.exitedAt)
			f.run(t)
			if _, ok := f.intent(); ok != tc.want {
				t.Fatalf("captured = %v, want %v", ok, tc.want)
			}
		})
	}
}

func TestCaptureRuleBIsOffWithoutAPreviousRun(t *testing.T) {
	f := newCaptureFixture(t, domain.ActivityExited, captureBoot.Add(-5*time.Second))
	f.m.SetBootWindow(domain.NewBootWindow("boot-1", captureBoot, nil, time.Time{}))
	f.run(t)
	if _, ok := f.intent(); ok {
		t.Fatal("an exit was captured with no previous daemon run to anchor it")
	}
}

func TestCaptureNeverTakesAStoppedOrProtectedSession(t *testing.T) {
	cases := map[string]func(*captureFixture){
		"stopped on purpose": func(f *captureFixture) {
			f.st.stopReasons = map[domain.SessionID]domain.StopReason{"mer-1": domain.StopReasonUser}
		},
		"policy never":      func(f *captureFixture) { f.st.protected = map[domain.SessionID]bool{"mer-1": true} },
		"policy unreadable": func(f *captureFixture) { f.st.policyUnreadable = true },
		"hibernated": func(f *captureFixture) {
			r := f.st.sessions["mer-1"]
			now := captureBoot
			r.HibernatedAt = &now
			f.st.sessions["mer-1"] = r
			f.rec = r
		},
		"terminated": func(f *captureFixture) {
			r := f.st.sessions["mer-1"]
			r.IsTerminated = true
			f.st.sessions["mer-1"] = r
			f.rec = r
		},
		"no provider handle": func(f *captureFixture) {
			r := f.st.sessions["mer-1"]
			r.Metadata.ProviderConversationID = ""
			f.st.sessions["mer-1"] = r
			f.rec = r
		},
		"failed version switch": func(f *captureFixture) {
			r := f.st.sessions["mer-1"]
			at := captureBoot
			r.StaleBinary = &domain.SessionStaleBinary{SwitchFailedAt: &at}
			f.st.sessions["mer-1"] = r
			f.rec = r
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newCaptureFixture(t, domain.ActivityActive, captureBoot.Add(-time.Hour))
			mutate(f)
			_ = f.m.checkSessionHealth(ctx, f.rec)
			if _, ok := f.intent(); ok {
				t.Fatal("a session that must not auto-resume was captured")
			}
		})
	}
}

func TestCaptureWithoutABootWindowCapturesNothing(t *testing.T) {
	f := newCaptureFixture(t, domain.ActivityActive, captureBoot.Add(-time.Hour))
	f.m.resumeCapture = nil
	f.run(t)
	if _, ok := f.intent(); ok {
		t.Fatal("capture ran without the daemon arming it")
	}
}

func TestCaptureMainCoordinatorIsAlwaysWorking(t *testing.T) {
	f := newCaptureFixture(t, domain.ActivityIdle, captureBoot.Add(-time.Hour))
	f.st.coordination = domain.CoordinationConfig{CoordinatorProjectID: chatTestProject}
	rec := f.st.sessions["mer-1"]
	rec.Kind = domain.KindOrchestrator
	f.st.sessions["mer-1"] = rec
	f.rec = rec
	f.run(t)
	if in, ok := f.intent(); !ok || in.Class != domain.ResumeWorking {
		t.Fatalf("intent = %+v ok=%v, want the main coordinator captured as working", in, ok)
	}
}

func TestCaptureRuleBServingAnOpenAutonomousStoryIsWorking(t *testing.T) {
	cases := []struct {
		name  string
		story domain.Story
		want  domain.ResumeClass
	}{
		{"open autonomous story", domain.Story{ID: "s1", Autonomous: true, Status: "in_progress", Assignments: []domain.StoryAssignment{{SessionID: "mer-1"}}}, domain.ResumeWorking},
		{"done story", domain.Story{ID: "s1", Autonomous: true, Status: "done", Assignments: []domain.StoryAssignment{{SessionID: "mer-1"}}}, domain.ResumeIdle},
		{"not autonomous", domain.Story{ID: "s1", Status: "in_progress", Assignments: []domain.StoryAssignment{{SessionID: "mer-1"}}}, domain.ResumeIdle},
		{"someone else's story", domain.Story{ID: "s1", Autonomous: true, Status: "in_progress", Assignments: []domain.StoryAssignment{{SessionID: "other-1"}}}, domain.ResumeIdle},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newCaptureFixture(t, domain.ActivityExited, captureBoot.Add(-5*time.Second))
			f.st.stories = []domain.Story{tc.story}
			f.run(t)
			if in, ok := f.intent(); !ok || in.Class != tc.want {
				t.Fatalf("intent = %+v ok=%v, want class %q", in, ok, tc.want)
			}
		})
	}
}

// A second boot before the first resume finished keeps the live intent untouched.
func TestCaptureKeepsALiveIntent(t *testing.T) {
	f := newCaptureFixture(t, domain.ActivityActive, captureBoot.Add(-time.Hour))
	f.st.intents = map[domain.SessionID]domain.ResumeIntent{"mer-1": {SessionID: "mer-1", BootID: "boot-1", State: domain.ResumeResuming, Attempts: 2}}
	f.run(t)
	if in := f.st.intents["mer-1"]; in.BootID != "boot-1" || in.Attempts != 2 || in.State != domain.ResumeResuming {
		t.Fatalf("a live intent was replaced: %+v", in)
	}
}

// The story list is read once per boot, with its own short context (not the first
// session's health-check deadline), and only a successful read is cached: a failure must
// not make every later session read "no story" for the rest of the boot.
func TestStoryListReadSurvivesACancelledCallerAndIsOnlyCachedOnSuccess(t *testing.T) {
	m, st, _ := newChatManager(&recordingLauncher{})
	m.SetBootWindow(captureWindow())
	st.stories = []domain.Story{{ID: "s1", Autonomous: true, Status: "in_progress", Assignments: []domain.StoryAssignment{{SessionID: "mer-1"}}}}
	capture := m.currentResumeCapture()

	// A caller whose deadline already expired (a slow per-session health check) still gets
	// the list: the read uses its own context.
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	if !capture.servesOpenStory(expired, m, "mer-1") {
		t.Fatal("an expired caller context made the story read fail")
	}

	// A failed read is not cached: the next session retries and succeeds.
	m2, st2, _ := newChatManager(&recordingLauncher{})
	m2.SetBootWindow(captureWindow())
	st2.stories = st.stories
	st2.storiesErr = errors.New("database is locked")
	capture2 := m2.currentResumeCapture()
	if capture2.servesOpenStory(ctx, m2, "mer-1") {
		t.Fatal("a failed read reported a story")
	}
	st2.storiesErr = nil
	if !capture2.servesOpenStory(ctx, m2, "mer-1") {
		t.Fatal("a later session still read 'no story' after one failed read")
	}
	reads := st2.storyReads
	capture2.servesOpenStory(ctx, m2, "mer-1")
	capture2.servesOpenStory(ctx, m2, "other-1")
	if st2.storyReads != reads {
		t.Fatalf("a successful read was not cached: %d reads after %d", st2.storyReads, reads)
	}
}
