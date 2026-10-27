package sessionmanager

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// probeAgent answers the adapter history probe.
type probeAgent struct {
	fakeAgent
	exists bool
	err    error
}

func (probeAgent) NativeConversationID(_ context.Context, _ ports.SessionRef, _ domain.SessionMode, id string) (string, bool, error) {
	return id, true, nil
}

func (a probeAgent) NativeConversationExists(context.Context, ports.SessionRef, string, map[string]string) (bool, error) {
	return a.exists, a.err
}

// handoffOnlyAgent reserves native ids but has no history probe.
type handoffOnlyAgent struct{ fakeAgent }

func (handoffOnlyAgent) NativeConversationID(_ context.Context, _ ports.SessionRef, _ domain.SessionMode, id string) (string, bool, error) {
	return id, true, nil
}

func TestProbeNativeHistoryOnlyAnExplicitExistsIsPresent(t *testing.T) {
	cases := []struct {
		name  string
		agent ports.Agent
		want  NativeHistoryProof
	}{
		{"the probe says it exists", probeAgent{exists: true}, NativeHistoryPresent},
		{"the probe says it is gone", probeAgent{exists: false}, NativeHistoryMissing},
		{"the probe fails", probeAgent{err: errors.New("transcript root unreadable")}, NativeHistoryUnproven},
		{"the adapter has a handoff but no probe", handoffOnlyAgent{}, NativeHistoryUnproven},
		{"the adapter has no handoff", fakeAgent{}, NativeHistoryUnproven},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, st, _ := newChatManager(&recordingLauncher{})
			m.agents = singleAgent{agent: tc.agent}
			seedChatResumeSession(st, domain.ActivityExited)
			if got := m.ProbeNativeHistory(ctx, "mer-1"); got != tc.want {
				t.Fatalf("proof = %v, want %v", got, tc.want)
			}
		})
	}
	m, st, _ := newChatManager(&recordingLauncher{})
	m.agents = singleAgent{agent: probeAgent{exists: true}}
	seedChatResumeSession(st, domain.ActivityExited)
	rec := st.sessions["mer-1"]
	rec.Metadata.ProviderConversationID = ""
	st.sessions["mer-1"] = rec
	if got := m.ProbeNativeHistory(ctx, "mer-1"); got != NativeHistoryUnproven {
		t.Fatalf("a session with no provider handle read %v, want unproven", got)
	}
	if got := m.ProbeNativeHistory(ctx, "ghost-1"); got != NativeHistoryUnproven {
		t.Fatalf("an unknown session read %v, want unproven", got)
	}
}

func TestParkResumeSessionWritesIdleAndTheMarkerWithoutLaunchingAnything(t *testing.T) {
	launcher := &recordingLauncher{}
	m, st, rt := newChatManager(launcher)
	m.clock = func() time.Time { return captureBoot }
	seedChatResumeSession(st, domain.ActivityExited)

	parked, err := m.ParkResumeSession(ctx, "mer-1")
	if err != nil || !parked {
		t.Fatalf("park = %v, %v; want parked", parked, err)
	}
	got := st.sessions["mer-1"]
	if got.HibernatedAt == nil || !got.HibernatedAt.Equal(captureBoot) || got.Activity.State != domain.ActivityIdle {
		t.Fatalf("parked row = hibernated %v, activity %q; want the marker and idle", got.HibernatedAt, got.Activity.State)
	}
	if len(launcher.started) != 0 || rt.created != 0 {
		t.Fatalf("parking launched something: controllers=%d runtimes=%d", len(launcher.started), rt.created)
	}
	// Parked twice is a no-op.
	if again, err := m.ParkResumeSession(ctx, "mer-1"); err != nil || again {
		t.Fatalf("second park = %v, %v; want not parked", again, err)
	}
}

func TestParkResumeSessionRefusesAnythingThatIsNotAnExitedAllowedChat(t *testing.T) {
	cases := map[string]func(*fakeStore, *recordingLauncher){
		"protected":         func(st *fakeStore, _ *recordingLauncher) { st.protected = map[domain.SessionID]bool{"mer-1": true} },
		"policy unreadable": func(st *fakeStore, _ *recordingLauncher) { st.policyUnreadable = true },
		"stopped on purpose": func(st *fakeStore, _ *recordingLauncher) {
			st.stopReasons = map[domain.SessionID]domain.StopReason{"mer-1": domain.StopReasonUser}
		},
		"terminated": func(st *fakeStore, _ *recordingLauncher) {
			r := st.sessions["mer-1"]
			r.IsTerminated = true
			st.sessions["mer-1"] = r
		},
		"already asleep": func(st *fakeStore, _ *recordingLauncher) {
			r := st.sessions["mer-1"]
			at := captureBoot
			r.HibernatedAt = &at
			st.sessions["mer-1"] = r
		},
		"not exited": func(st *fakeStore, _ *recordingLauncher) {
			r := st.sessions["mer-1"]
			r.Activity.State = domain.ActivityActive
			st.sessions["mer-1"] = r
		},
		"live controller": func(_ *fakeStore, l *recordingLauncher) { l.live = true },
		"a terminal session": func(st *fakeStore, _ *recordingLauncher) {
			r := st.sessions["mer-1"]
			r.Mode = domain.SessionModeTUI
			st.sessions["mer-1"] = r
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			launcher := &recordingLauncher{}
			m, st, _ := newChatManager(launcher)
			seedChatResumeSession(st, domain.ActivityExited)
			mutate(st, launcher)
			before := st.sessions["mer-1"]

			parked, err := m.ParkResumeSession(ctx, "mer-1")
			if parked {
				t.Fatalf("parked a session that must not be (err %v)", err)
			}
			if after := st.sessions["mer-1"]; after.Revision != before.Revision || after.Activity.State != before.Activity.State ||
				(after.HibernatedAt == nil) != (before.HibernatedAt == nil) {
				t.Fatalf("a refused park changed the row: before %+v after %+v", before, after)
			}
		})
	}
}

// A row that changed after it was captured reads as "not parked", and the caller
// retries with fresh state; contention is never reported as parked.
func TestParkResumeSessionReportsContentionAsNotParked(t *testing.T) {
	m, st, _ := newChatManager(&recordingLauncher{})
	seedChatResumeSession(st, domain.ActivityExited)
	st.parkRevisionBump = true
	if parked, err := m.ParkResumeSession(ctx, "mer-1"); err != nil || parked {
		t.Fatalf("park under contention = %v, %v; want not parked", parked, err)
	}
	if st.sessions["mer-1"].HibernatedAt != nil {
		t.Fatal("contention left a marker behind")
	}
}
