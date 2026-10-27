package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/reproof"
	sessionmanager "github.com/aoagents/agent-orchestrator/backend/internal/session_manager"
)

func TestABusyRelaunchIsNotAFailureForTheReproofService(t *testing.T) {
	busy := reproofManager{relaunch: func(context.Context, domain.SessionID) error {
		return errors.Join(errors.New("context"), sessionmanager.ErrRelaunchBusy)
	}}
	if err := busy.RelaunchForIdentity(context.Background(), "mer-1"); !errors.Is(err, reproof.ErrBusy) {
		t.Fatalf("error = %v, want reproof.ErrBusy", err)
	}
	other := errors.New("resume failed")
	failing := reproofManager{relaunch: func(context.Context, domain.SessionID) error { return other }}
	if err := failing.RelaunchForIdentity(context.Background(), "mer-1"); !errors.Is(err, other) || errors.Is(err, reproof.ErrBusy) {
		t.Fatalf("error = %v, want the real failure kept", err)
	}
}

// The production session manager must satisfy what newReproof asserts at runtime.
var _ interface {
	RelaunchForIdentity(ctx context.Context, id domain.SessionID) error
} = (*sessionmanager.Manager)(nil)
