package daemon

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/autoresume"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/reproof"
	sessionmanager "github.com/aoagents/agent-orchestrator/backend/internal/session_manager"
)

// reproofManager adapts the session manager to the re-proof service: a session that turned out
// not to be idle, or has background work running, is "busy" (try later) and a resume that failed
// after the stop is its own outcome; none of those is an ordinary failure.
type reproofManager struct {
	relaunch func(ctx context.Context, id domain.SessionID) (bool, error)
	resume   func(ctx context.Context, id domain.SessionID) (bool, error)
}

func (r reproofManager) RelaunchForIdentity(ctx context.Context, id domain.SessionID) (bool, error) {
	done, err := r.relaunch(ctx, id)
	switch {
	case errors.Is(err, sessionmanager.ErrRelaunchBusy):
		return false, reproof.ErrBusy
	case errors.Is(err, sessionmanager.ErrRelaunchWorkRunning):
		return false, reproof.ErrWorkRunning
	case errors.Is(err, sessionmanager.ErrRelaunchResumeFailed):
		return false, errors.Join(reproof.ErrResumeFailed, err)
	}
	return done, err
}

func (r reproofManager) ResumeStopped(ctx context.Context, id domain.SessionID) (bool, error) {
	return r.resume(ctx, id)
}

// reproofStore is what the stopped-session follow-up writes through.
type reproofStore interface {
	reproof.Store
	CaptureResumeIntent(ctx context.Context, fresh domain.ResumeIntent) (domain.ResumeIntent, bool, error)
}

// reproofStopped makes a session a relaunch left down impossible to lose: a durable ED7 resume
// intent (the auto-resume pass of the next daemon start revives it) and, when the service gives up,
// the standard "work has stopped" notice to the coordinator conversation.
type reproofStopped struct {
	store  reproofStore
	notice autoresume.NoticeWriter
	bootID string
	now    func() time.Time
}

func (r reproofStopped) RecordStopped(ctx context.Context, id domain.SessionID) error {
	_, _, err := r.store.CaptureResumeIntent(ctx, domain.ResumeIntent{
		SessionID: id, BootID: r.bootID, CapturedAt: r.now(), UpdatedAt: r.now(),
		Class: domain.ResumeWorking, PriorActivity: domain.ActivityIdle, State: domain.ResumePending,
	})
	return err
}

func (r reproofStopped) NotifyGaveUp(ctx context.Context, id domain.SessionID, reason string) error {
	return r.notice.NotifyRelaunchFailure(ctx, id, reason, r.bootID)
}

// newReproof builds the rolling re-proof service (ED8): it relaunches idle sessions that have no
// identity proof, coordinator first. AO_REPROOF=off disables it (scratch/diagnostic use).
func newReproof(log *slog.Logger, mgr any, store reproofStore, notice autoresume.NoticeWriter) *reproof.Service {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("AO_REPROOF")), "off") {
		log.Info("rolling re-proof disabled by AO_REPROOF=off")
		return nil
	}
	relauncher, ok := mgr.(interface {
		RelaunchForIdentity(ctx context.Context, id domain.SessionID) (bool, error)
		ResumeStoppedForIdentity(ctx context.Context, id domain.SessionID) (bool, error)
	})
	if !ok {
		log.Warn("rolling re-proof unavailable: the session manager cannot relaunch for identity")
		return nil
	}
	stopped := reproofStopped{store: store, notice: notice, bootID: "reproof-" + time.Now().UTC().Format("20060102T150405"), now: time.Now}
	return reproof.New(reproofManager{relaunch: relauncher.RelaunchForIdentity, resume: relauncher.ResumeStoppedForIdentity}, store, stopped, log, reproof.Config{})
}
