package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/reproof"
	sessionmanager "github.com/aoagents/agent-orchestrator/backend/internal/session_manager"
)

func TestTheAdapterMapsEachRelaunchOutcomeToItsOwnReproofError(t *testing.T) {
	cases := map[string]struct {
		err  error
		want error
	}{
		"busy":          {errors.Join(errors.New("context"), sessionmanager.ErrRelaunchBusy), reproof.ErrBusy},
		"work running":  {sessionmanager.ErrRelaunchWorkRunning, reproof.ErrWorkRunning},
		"resume failed": {errors.Join(errors.New("cause"), sessionmanager.ErrRelaunchResumeFailed), reproof.ErrResumeFailed},
	}
	for name, tc := range cases {
		m := reproofManager{relaunch: func(context.Context, domain.SessionID) (bool, error) { return false, tc.err }}
		if _, err := m.RelaunchForIdentity(context.Background(), "mer-1"); !errors.Is(err, tc.want) {
			t.Errorf("%s: error = %v, want %v", name, err, tc.want)
		}
	}
	other := errors.New("some other failure")
	failing := reproofManager{relaunch: func(context.Context, domain.SessionID) (bool, error) { return false, other }}
	_, err := failing.RelaunchForIdentity(context.Background(), "mer-1")
	if !errors.Is(err, other) || errors.Is(err, reproof.ErrBusy) || errors.Is(err, reproof.ErrResumeFailed) || errors.Is(err, reproof.ErrWorkRunning) {
		t.Fatalf("error = %v, want the real failure kept and not mistaken for a skip", err)
	}
	ok := reproofManager{relaunch: func(context.Context, domain.SessionID) (bool, error) { return true, nil }}
	if done, err := ok.RelaunchForIdentity(context.Background(), "mer-1"); !done || err != nil {
		t.Fatalf("success = %v, %v", done, err)
	}
}

// The production session manager must satisfy what newReproof asserts at runtime.
var _ interface {
	RelaunchForIdentity(ctx context.Context, id domain.SessionID) (bool, error)
	ResumeStoppedForIdentity(ctx context.Context, id domain.SessionID) (bool, error)
} = (*sessionmanager.Manager)(nil)

type captureStore struct {
	reproof.Store
	got []domain.ResumeIntent
}

func (c *captureStore) CaptureResumeIntent(_ context.Context, fresh domain.ResumeIntent) (domain.ResumeIntent, bool, error) {
	c.got = append(c.got, fresh)
	return fresh, true, nil
}

// A stopped session gets a durable, pending, working-class ED7 resume intent, so the next daemon
// start revives it even if this daemon dies first.
func TestARelaunchThatLeftASessionDownRecordsAResumeIntent(t *testing.T) {
	store := &captureStore{}
	at := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	stopped := reproofStopped{store: store, bootID: "reproof-x", now: func() time.Time { return at }}
	if err := stopped.RecordStopped(context.Background(), "mer-1"); err != nil {
		t.Fatal(err)
	}
	if len(store.got) != 1 {
		t.Fatalf("recorded %d intents, want 1", len(store.got))
	}
	in := store.got[0]
	if in.SessionID != "mer-1" || in.State != domain.ResumePending || in.Class != domain.ResumeWorking || in.BootID != "reproof-x" || !in.CapturedAt.Equal(at) {
		t.Fatalf("intent = %+v", in)
	}
}
