package daemon

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/reproof"
	sessionmanager "github.com/aoagents/agent-orchestrator/backend/internal/session_manager"
)

// reproofManager adapts the session manager to the re-proof service: a session that turned out
// not to be idle is "busy" (try later), not a failure.
type reproofManager struct {
	relaunch func(ctx context.Context, id domain.SessionID) error
}

func (r reproofManager) RelaunchForIdentity(ctx context.Context, id domain.SessionID) error {
	err := r.relaunch(ctx, id)
	if errors.Is(err, sessionmanager.ErrRelaunchBusy) {
		return reproof.ErrBusy
	}
	return err
}

// newReproof builds the rolling re-proof service (ED8): it relaunches idle sessions that have no
// identity proof, coordinator first. AO_REPROOF=off disables it (scratch/diagnostic use).
func newReproof(log *slog.Logger, mgr any, store reproof.Store) *reproof.Service {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("AO_REPROOF")), "off") {
		log.Info("rolling re-proof disabled by AO_REPROOF=off")
		return nil
	}
	relauncher, ok := mgr.(interface {
		RelaunchForIdentity(ctx context.Context, id domain.SessionID) error
	})
	if !ok {
		log.Warn("rolling re-proof unavailable: the session manager cannot relaunch for identity")
		return nil
	}
	return reproof.New(reproofManager{relaunch: relauncher.RelaunchForIdentity}, store, log, reproof.Config{})
}
