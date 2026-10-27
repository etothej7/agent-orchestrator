package sessionmanager

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/identity"
)

// hibernatingLauncher is a chat launcher whose provider can be put to sleep for the relaunch.
type hibernatingLauncher struct {
	*recordingLauncher
	store      *fakeStore
	hibernated []domain.SessionID
}

func (h *hibernatingLauncher) HibernateChatForUpdate(_ context.Context, id domain.SessionID) (bool, error) {
	rec := h.store.sessions[id]
	at := time.Now().UTC()
	rec.HibernatedAt = &at
	h.store.sessions[id] = rec
	h.hibernated = append(h.hibernated, id)
	return true, nil
}

// A relaunch for identity never touches a session that is mid-turn or waiting on a person.
func TestRelaunchForIdentityNeverTouchesASessionThatIsNotIdle(t *testing.T) {
	for name, state := range map[string]domain.ActivityState{"a running turn": domain.ActivityActive, "an approval": domain.ActivityWaitingInput, "blocked": domain.ActivityBlocked} {
		t.Run(name, func(t *testing.T) {
			base := &recordingLauncher{}
			mgr, store, _ := newChatManager(base)
			launcher := &hibernatingLauncher{recordingLauncher: base, store: store}
			mgr.chat = launcher
			seedChatResumeSession(store, state)
			err := mgr.RelaunchForIdentity(context.Background(), "mer-1")
			if !errors.Is(err, ErrRelaunchBusy) {
				t.Fatalf("error = %v, want ErrRelaunchBusy", err)
			}
			if len(launcher.hibernated) != 0 || len(base.started) != 0 {
				t.Fatalf("a busy session was stopped (%v) or started (%d)", launcher.hibernated, len(base.started))
			}
		})
	}
}

func TestRelaunchForIdentityLeavesTerminatedAndSleepingSessionsAlone(t *testing.T) {
	base := &recordingLauncher{}
	mgr, store, _ := newChatManager(base)
	launcher := &hibernatingLauncher{recordingLauncher: base, store: store}
	mgr.chat = launcher
	seedChatResumeSession(store, domain.ActivityIdle)
	rec := store.sessions["mer-1"]
	rec.IsTerminated = true
	store.sessions["mer-1"] = rec
	if err := mgr.RelaunchForIdentity(context.Background(), "mer-1"); err != nil {
		t.Fatalf("a terminated session = %v, want a quiet no-op", err)
	}
	rec.IsTerminated = false
	at := time.Now().UTC()
	rec.HibernatedAt = &at
	store.sessions["mer-1"] = rec
	if err := mgr.RelaunchForIdentity(context.Background(), "mer-1"); err != nil {
		t.Fatalf("a sleeping session = %v, want a quiet no-op (its wake issues the proof)", err)
	}
	if len(launcher.hibernated) != 0 || len(base.started) != 0 {
		t.Fatalf("stopped %v started %d for sessions that need nothing", launcher.hibernated, len(base.started))
	}
}

// countingIssuer counts how many proofs a relaunch mints.
type countingIssuer struct {
	inner ProofIssuer
	n     int
}

func (c *countingIssuer) Issue(ctx context.Context, id domain.SessionID) (string, error) {
	c.n++
	return c.inner.Issue(ctx, id)
}

// An idle chat session is put to sleep and started again, resuming its conversation; the launch
// environment (built when the provider really starts) mints exactly ONE new proof.
func TestRelaunchForIdentityRestartsAnIdleSessionWithExactlyOneNewProof(t *testing.T) {
	base := &recordingLauncher{}
	mgr, store, _ := newChatManager(base)
	launcher := &hibernatingLauncher{recordingLauncher: base, store: store}
	mgr.chat = launcher
	proofs := &memoryProofStore{hashes: map[domain.SessionID]string{}}
	issuer := &countingIssuer{inner: identity.NewIssuer(proofs, nil)}
	mgr.proofs = issuer
	seedChatResumeSession(store, domain.ActivityIdle)

	if err := mgr.RelaunchForIdentity(context.Background(), "mer-1"); err != nil {
		t.Fatalf("RelaunchForIdentity: %v", err)
	}
	if len(launcher.hibernated) != 1 || len(base.started) != 1 {
		t.Fatalf("hibernated %v, started %d; want one stop and one start", launcher.hibernated, len(base.started))
	}
	if issuer.n != 1 || proofs.hashes["mer-1"] == "" {
		t.Fatalf("proofs issued = %d (held: %v); want exactly one, held by the daemon", issuer.n, proofs.hashes["mer-1"] != "")
	}
}
