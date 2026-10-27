package sessionmanager

import (
	"context"
	"sync"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// resumeCaptureStore is the optional part of the store that records resume intents.
// A store without it (the test fakes) captures nothing.
type resumeCaptureStore interface {
	AutoResumeAllowed(ctx context.Context, id domain.SessionID) bool
	StopReason(ctx context.Context, id domain.SessionID) (domain.StopReason, error)
	InterruptedHumanTurns(ctx context.Context, id domain.SessionID, windowStart, bootStart time.Time) ([]domain.InterruptedTurn, error)
	CaptureResumeIntent(ctx context.Context, fresh domain.ResumeIntent) (domain.ResumeIntent, bool, error)
}

// resumeCapture is the boot-time state capture needs: the window the previous
// daemon run left, and, read once, the sessions that serve unfinished autonomous
// stories.
type resumeCapture struct {
	window domain.BootWindow

	storiesOnce    sync.Once
	storySessions  map[domain.SessionID]bool
	storiesReadErr error
}

// SetBootWindow arms resume-intent capture for this boot. The daemon calls it with
// the window it read BEFORE any startup write, ahead of the health pass. Without it
// nothing is captured.
func (m *Manager) SetBootWindow(window domain.BootWindow) {
	m.resumeCaptureMu.Lock()
	defer m.resumeCaptureMu.Unlock()
	m.resumeCapture = &resumeCapture{window: window}
}

func (m *Manager) currentResumeCapture() *resumeCapture {
	m.resumeCaptureMu.Lock()
	defer m.resumeCaptureMu.Unlock()
	return m.resumeCapture
}

// captureResumeIntent records, before boot marks the session exited, that a chat
// session lost its agent in a restart. It never changes the row and never fails
// boot: a problem is logged and the session simply stays exited, as before.
//
// Candidates (design section 3.3): the policy allows it, nothing stopped it on
// purpose, and either (A) its prior activity is not exited (kill -9, power loss,
// daemon-first shutdown: nothing wrote exited) or (B) it is exited with no stop
// reason and the exit was written inside the previous daemon's shutdown window.
func (m *Manager) captureResumeIntent(ctx context.Context, rec domain.SessionRecord) {
	capture := m.currentResumeCapture()
	store, ok := m.store.(resumeCaptureStore)
	if capture == nil || !ok {
		return
	}
	if rec.IsTerminated || rec.HibernatedAt != nil || domain.NormalizeSessionMode(rec.Mode) != domain.SessionModeChat ||
		rec.Metadata.ProviderConversationID == "" ||
		(rec.StaleBinary != nil && rec.StaleBinary.SwitchFailedAt != nil) {
		return
	}
	window := capture.window
	prior := rec.Activity.State
	ruleA := prior != domain.ActivityExited
	ruleB := prior == domain.ActivityExited && window.InShutdownWindow(rec.Activity.LastActivityAt)
	if !ruleA && !ruleB {
		return
	}
	// Fail closed: an unreadable policy or stop reason is "do not resume".
	if !store.AutoResumeAllowed(ctx, rec.ID) {
		return
	}
	if reason, err := store.StopReason(ctx, rec.ID); err != nil || reason != domain.StopReasonNone {
		if err != nil {
			m.logger.Warn("auto-resume: stop reason unreadable, not capturing", "sessionID", rec.ID, "error", err)
		}
		return
	}
	windowStart := window.BootStart // an empty window: only turns still queued or running
	if window.RuleBEnabled() {
		windowStart = window.ShutdownWindowStart()
	}
	turns, err := store.InterruptedHumanTurns(ctx, rec.ID, windowStart, window.BootStart)
	if err != nil {
		m.logger.Warn("auto-resume: interrupted turns unreadable, not capturing", "sessionID", rec.ID, "error", err)
		return
	}
	now := m.clock()
	intent := domain.ResumeIntent{
		SessionID: rec.ID, BootID: window.BootID, CapturedAt: now,
		Class:         m.resumeClass(ctx, capture, rec, prior, ruleB, len(turns) > 0),
		PriorActivity: prior, InterruptedTurns: turns, State: domain.ResumePending, UpdatedAt: now,
	}
	stored, created, err := store.CaptureResumeIntent(ctx, intent)
	if err != nil {
		m.logger.Warn("auto-resume: could not record resume intent", "sessionID", rec.ID, "error", err)
		return
	}
	m.logger.Info("auto-resume: captured", "sessionID", rec.ID, "class", stored.Class, "priorActivity", prior,
		"ruleB", ruleB, "interruptedTurns", len(stored.InterruptedTurns), "new", created)
}

// resumeClass is working for a session that was mid-work, had a human turn cut off,
// serves an unfinished autonomous story (Rule B: nothing else says it was working),
// or is the main coordinator (Ed's single point of contact must never 409); idle
// otherwise.
func (m *Manager) resumeClass(ctx context.Context, capture *resumeCapture, rec domain.SessionRecord,
	prior domain.ActivityState, ruleB, hasTurns bool,
) domain.ResumeClass {
	switch {
	case prior == domain.ActivityActive || prior == domain.ActivityWaitingInput || prior == domain.ActivityBlocked,
		hasTurns:
		return domain.ResumeWorking
	}
	if coordinator, err := m.isMainCoordinator(ctx, rec.ID); err == nil && coordinator {
		return domain.ResumeWorking
	}
	if ruleB && capture.servesOpenStory(ctx, m, rec.ID) {
		return domain.ResumeWorking
	}
	return domain.ResumeIdle
}

// servesOpenStory reports whether the session is assigned to a story that is
// autonomous and not done. The stories are read once per boot; an unreadable list
// reads as "no" (the session then parks idle, which a message still wakes).
func (c *resumeCapture) servesOpenStory(ctx context.Context, m *Manager, id domain.SessionID) bool {
	c.storiesOnce.Do(func() {
		lister, ok := m.store.(interface {
			ListStories(context.Context) ([]domain.Story, error)
		})
		if !ok {
			return
		}
		stories, err := lister.ListStories(ctx)
		if err != nil {
			c.storiesReadErr = err
			m.logger.Warn("auto-resume: stories unreadable, idle sessions will park", "error", err)
			return
		}
		c.storySessions = map[domain.SessionID]bool{}
		for _, story := range stories {
			if !story.Autonomous || story.Status == "done" {
				continue
			}
			for _, assignment := range story.Assignments {
				c.storySessions[assignment.SessionID] = true
			}
		}
	})
	return c.storySessions[id]
}
