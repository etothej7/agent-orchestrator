package story

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/coordination"
)

const continuationLimit = 3

// ContinuationStore reuses story/session facts and a small durable CAS ledger.
type ContinuationStore interface {
	Store
	ListSessions(context.Context, domain.ProjectID) ([]domain.SessionRecord, error)
	GetStoryContinuation(context.Context, string) (domain.StoryContinuation, bool, error)
	SaveStoryContinuation(context.Context, domain.StoryContinuation) (bool, error)
	ClaimStoryContinuation(context.Context, domain.StoryContinuation, int64) (bool, error)
	ResumeStoryContinuation(context.Context, domain.StoryContinuation, int64, domain.StoryEvent) (bool, error)
}

// ContinuationSender uses normal idempotent chat delivery.
type ContinuationSender interface {
	Send(context.Context, domain.SessionID, ports.ChatUserMessage) (domain.ConversationTurn, error)
}

// Continuations wakes responsible managers; it never invents worker results or
// advances story status. Agent inactivity alone is not completion or failure.
type Continuations struct {
	Store   ContinuationStore
	Stories *Service
	Sender  ContinuationSender
	Now     func() time.Time
}

func (c *Continuations) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

// RunOnce is safe to race with another sweep: every send owns a durable lease.
func (c *Continuations) RunOnce(ctx context.Context) error {
	stories, err := c.Stories.List(ctx)
	if err != nil {
		return err
	}
	var failures []error
	for _, s := range stories {
		if !s.Autonomous {
			continue
		}
		if err := c.reconcile(ctx, s); err != nil {
			failures = append(failures, fmt.Errorf("continue story %s: %w", s.ID, err))
		}
	}
	return errors.Join(failures...)
}

func (c *Continuations) reconcile(ctx context.Context, s domain.Story) error {
	ctx, release := coordination.ReadLock(ctx, c.Store)
	defer release()
	now := c.now()
	rec, found, err := c.Store.GetStoryContinuation(ctx, s.ID)
	if err != nil {
		return err
	}
	if found && rec.LeaseUntil.After(now) {
		return nil
	}
	// A question pauses dispatch without destroying a possibly accepted request.
	if s.Question != "" && found {
		return nil
	}
	// Terminal states require a durable story fact, never an idle provider.
	if s.Status == "done" || s.Question != "" {
		state := "completed"
		if s.Question != "" {
			state = "waiting_for_ed"
		}
		if rec.State == state {
			return nil
		}
		rec.StoryID = s.ID
		rec.State = state
		rec.LeaseUntil = time.Time{}
		_, err = c.Store.SaveStoryContinuation(ctx, rec)
		return err
	}
	rec.StoryID = s.ID
	cfg, err := c.Store.GetCoordination(ctx)
	if err != nil {
		return err
	}
	for _, projectID := range s.ProjectIDs {
		if !cfg.Manages(projectID) {
			if rec.State == "blocked" {
				return c.notifyBlocked(ctx, s, rec)
			}
			return c.block(ctx, rec, "The story is outside the current coordinator repository scope. Restore its authorized scope before resuming.")
		}
		project, found, err := c.Store.GetProject(ctx, string(projectID))
		if err != nil {
			return err
		}
		if !found || !project.ArchivedAt.IsZero() {
			if rec.State == "blocked" {
				return c.notifyBlocked(ctx, s, rec)
			}
			return c.block(ctx, rec, "A repository in this story is unavailable or archived. Restore the authorized repository before resuming.")
		}
	}
	if rec.State == "blocked" {
		return c.notifyBlocked(ctx, s, rec)
	} // Explicit human recovery is required.
	events, err := c.Store.StoryEvents(ctx, s.ID)
	if err != nil {
		return err
	}
	checkpoint := continuationCheckpoint(s, events)
	if rec.State == "waiting" {
		turn, err := c.Store.TurnByID(ctx, rec.TurnID)
		if err != nil {
			return c.deferOrBlock(ctx, rec, now, "The manager turn cannot be inspected. Restore its durable state before deciding whether to retry.")
		} // Unknown is not dead, failed, or safe to redispatch.
		switch turn.State {
		case domain.TurnStateQueued, domain.TurnStateRunning:
			return c.deferOrBlock(ctx, rec, now, "The assigned manager has not completed its turn. Check its pending provider or permission blocker.")
		case domain.TurnStateInterrupted, domain.TurnStateCancelled:
			return c.block(ctx, rec, "The manager turn was stopped or cancelled. Confirm whether this story should resume; automation will not override that stop.")
		case domain.TurnStateCompleted, domain.TurnStateFailed, domain.TurnStateRecovered:
			if checkpoint == rec.Checkpoint && rec.ProgressAttempts >= continuationLimit {
				return c.block(ctx, rec, "The manager completed three continuation turns without advancing this story. Decide whether to clarify scope, change the responsible agent, or stop the work.")
			}
			rec.State = "ready"
			rec.DeliveryAttempts = 0
			rec.DeliveryID = ""
			rec.TurnID = ""
			rec.WaitingSince = time.Time{}
		default:
			return c.deferOrBlock(ctx, rec, now, "The manager turn state is unknown. Inspect its controller before deciding whether to retry.")
		}
	}
	// Do not discard an uncertain in-flight delivery, even when progress changed.
	if rec.State != "sending" && rec.Checkpoint != checkpoint {
		rec = domain.StoryContinuation{StoryID: s.ID, Revision: rec.Revision, RecoveryID: rec.RecoveryID, Generation: rec.Generation + 1, Checkpoint: checkpoint, State: "ready"}
	}
	// A new status/report wakes a settled phase, not a new progress budget.
	// Consume its cursor only with a fresh dispatch, never while the owner is
	// still busy or an earlier send is uncertain.
	phaseReportID := rec.PhaseReportID
	phaseChanged, phaseWake := false, false
	if rec.State == "ready" {
		phaseReportID = ""
		for _, event := range events {
			if event.Kind == "report" && (event.Result == "done" || event.Result == "needs_input" || event.Result == "stuck") {
				phaseReportID = event.ID
			}
		}
		phaseChanged = rec.PhaseStatus != s.Status || rec.PhaseReportID != phaseReportID
		phaseWake = (phaseReportID != "" && rec.PhaseReportID != phaseReportID) || (rec.PhaseStatus != "" && rec.PhaseStatus != s.Status)
	}
	if rec.State == "ready" && (phaseChanged || !rec.WaitingSince.IsZero()) {
		_, busy, err := c.owner(ctx, s, phaseWake)
		if err != nil {
			return err
		}
		if !busy {
			rec.NextAt = time.Time{}
			rec.WaitingSince = time.Time{}
		}
	}
	if rec.NextAt.After(now) {
		return nil
	}
	if rec.State != "sending" {
		target, busy, err := c.owner(ctx, s, phaseWake)
		if err != nil {
			return err
		}
		if target == "" {
			return c.block(ctx, rec, "No active responsible manager or overall coordinator is available. Restore the coordinator or assign a manager to continue.")
		}
		if busy {
			return c.deferOrBlock(ctx, rec, now, "An assigned agent remains active or blocked without advancing the story. Resolve its provider/permission blocker or change the assignment.")
		}
		rec.PhaseStatus = s.Status
		rec.PhaseReportID = phaseReportID
		rec.RecipientSessionID = target
		rec.ProgressAttempts++
		rec.DeliveryID = fmt.Sprintf("story-continue-%x-%d-%d", sha256.Sum256([]byte(s.ID+"\x00"+checkpoint)), rec.Generation, rec.ProgressAttempts)
		rec.Text = fmt.Sprintf("AO internal story continuation. Story %s; required next checkpoint: %s. Inspect its current public story events and responsible actors. As manager/coordinator, perform or delegate the next authorized step through normal AO commands; reuse existing sessions and stable story operation IDs. Inspect actual worker/reviewer output, route findings and re-review changed revisions. Record truthful evidence/status. Do not report Done from idle/receipt/report completion. Ask Ed only for a genuinely blocking scope decision; keep status/receipt chatter internal. Do not expand publication or installation authorization.", s.ID, checkpoint)
		rec.State = "sending"
	}
	if rec.DeliveryAttempts >= continuationLimit {
		return c.block(ctx, rec, "Delivery could not be confirmed after three attempts. Inspect the retained delivery before confirming a bounded retry; do not change the recipient while its outcome is uncertain.")
	}
	rec.DeliveryAttempts++
	rec.LeaseUntil = now.Add(30 * time.Second)
	saved, err := c.Store.ClaimStoryContinuation(ctx, rec, s.Revision)
	if err != nil || !saved {
		return err
	}
	rec.Revision++
	sendCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	turn, sendErr := c.Sender.Send(sendCtx, rec.RecipientSessionID, ports.ChatUserMessage{Text: rec.Text, ClientMessageID: rec.DeliveryID, Origin: domain.MessageOriginAutomation})
	rec.LeaseUntil = time.Time{}
	rec.NextAt = now.Add(time.Minute)
	if sendErr != nil || turn.ID == "" {
		// Keep both handle and original payload on every uncertain transport result.
		rec.Reason = "Delivery not confirmed; retry retains the original request."
	} else {
		rec.State = "waiting"
		rec.TurnID = turn.ID
		rec.WaitingSince = now
		rec.Reason = ""
	}
	_, err = c.Store.SaveStoryContinuation(ctx, rec)
	return err
}

func (c *Continuations) block(ctx context.Context, rec domain.StoryContinuation, reason string) error {
	rec.State = "blocked"
	rec.Reason = reason
	rec.LeaseUntil = time.Time{}
	_, err := c.Store.SaveStoryContinuation(ctx, rec)
	return err
}
func (c *Continuations) deferOrBlock(ctx context.Context, rec domain.StoryContinuation, now time.Time, reason string) error {
	if rec.NextAt.After(now) {
		return nil
	} // CDC from our own ledger write is not a new retry deadline.
	if rec.WaitingSince.IsZero() {
		rec.WaitingSince = now
	}
	if now.Sub(rec.WaitingSince) >= 30*time.Minute {
		return c.block(ctx, rec, reason)
	}
	rec.NextAt = now.Add(time.Minute)
	_, err := c.Store.SaveStoryContinuation(ctx, rec)
	return err
}

func (c *Continuations) owner(ctx context.Context, s domain.Story, phaseWake bool) (domain.SessionID, bool, error) {
	var manager domain.SessionID
	busy := false
	for _, a := range s.Assignments {
		session, found, err := c.Store.GetSession(ctx, a.SessionID)
		if err != nil {
			return "", false, err
		}
		if !found || session.IsTerminated || session.Activity.State == domain.ActivityExited {
			continue
		}
		// A blocked worker needs its manager to investigate. Do not inject
		// into a manager's own active turn or permission dialog.
		// Phase reports/status transitions are new manager input even if a
		// worker is still finishing its turn. The recipient's own busy guard remains.
		if !phaseWake || a.Role == "manager" {
			busy = busy || session.Activity.State == domain.ActivityActive || session.ProvisionState.IsProvisioning() || (a.Role == "manager" && session.Activity.State == domain.ActivityBlocked)
		}
		if a.Role == "manager" {
			manager = a.SessionID
			open, err := c.hasOpenTurn(ctx, a.SessionID)
			if err != nil {
				return "", false, err
			}
			busy = busy || open
		}
	}
	if manager != "" {
		return manager, busy, nil
	}
	cfg, err := c.Store.GetCoordination(ctx)
	if err != nil {
		return "", false, err
	}
	if cfg.CoordinatorProjectID == "" {
		return "", busy, nil
	}
	sessions, err := c.Store.ListSessions(ctx, cfg.CoordinatorProjectID)
	if err != nil {
		return "", false, err
	}
	var selected domain.SessionRecord
	for _, session := range sessions {
		if session.Kind == domain.KindOrchestrator && !session.IsTerminated && session.Activity.State != domain.ActivityExited && (selected.ID == "" || session.CreatedAt.After(selected.CreatedAt)) {
			selected = session
		}
	}
	open, err := c.hasOpenTurn(ctx, selected.ID)
	if err != nil {
		return "", false, err
	}
	return selected.ID, busy || open || selected.Activity.State == domain.ActivityActive || selected.Activity.State == domain.ActivityBlocked || selected.ProvisionState.IsProvisioning(), nil
}

// hasOpenTurn closes the gap before a provider reports a freshly queued turn as
// active: a continuation sent then would only queue a redundant turn behind the
// work it is meant to prompt. Stores without turn history keep activity-only
// behavior.
func (c *Continuations) hasOpenTurn(ctx context.Context, id domain.SessionID) (bool, error) {
	reader, ok := c.Store.(interface {
		SessionHasOpenTurn(context.Context, domain.SessionID) (bool, error)
	})
	if !ok || id == "" {
		return false, nil
	}
	return reader.SessionHasOpenTurn(ctx, id)
}

// Only actual phase/evidence changes reset the bounded progress budget. Reports,
// assistant prose and repeated status claims do not constitute progress.
func continuationCheckpoint(s domain.Story, events []domain.StoryEvent) string {
	manager, worker, reviewer := false, false, false
	for _, a := range s.Assignments {
		switch a.Role {
		case "manager":
			manager = true
		case "worker":
			worker = true
		case "reviewer":
			reviewer = true
		}
	}
	if !manager {
		return "assign-manager"
	}
	if !worker {
		return "assign-worker"
	}
	var implementation string
	for _, e := range events {
		if e.Kind == "implementation" {
			implementation = e.Reference
		}
	}
	if implementation == "" {
		return "implementation"
	}
	passed := false
	review := ""
	reviewID := ""
	for _, e := range events {
		if e.Reference != implementation {
			continue
		}
		switch e.Kind {
		case "testing":
			passed = e.Result == "passed"
		case "review_passed", "review_failed":
			review = e.Kind
			reviewID = e.ID
		}
	}
	if review == "review_failed" {
		return "remediate:" + implementation + ":" + reviewID
	}
	if !passed {
		return "verify:" + implementation
	}
	if !reviewer {
		return "assign-reviewer:" + implementation
	}
	if !ready(events) {
		return "independent-review:" + implementation
	}
	return "complete-scope:" + implementation
}

// Status exposes only the decision needed by a human, not delivery prompts or
// provider payloads. Questions pause an outstanding receipt without erasing it.
func (c *Continuations) Status(ctx context.Context, id string) (domain.StoryContinuationStatus, error) {
	story, err := c.Store.GetStory(ctx, id)
	if err != nil {
		return domain.StoryContinuationStatus{}, err
	}
	rec, _, err := c.Store.GetStoryContinuation(ctx, id)
	if err != nil {
		return domain.StoryContinuationStatus{}, err
	}
	status := domain.StoryContinuationStatus{StoryID: id, Revision: rec.Revision, State: rec.State, Checkpoint: rec.Checkpoint, RecipientSessionID: rec.RecipientSessionID, Reason: publicContinuationReason(rec)}
	if !story.Autonomous {
		status.State = "manual"
	} else if story.Status == "done" {
		status.State = "completed"
	} else if story.Question != "" {
		status.State = "waiting_for_ed"
	} else if status.State == "" {
		status.State = "ready"
	}
	return status, nil
}

// Resume is an explicit bounded retry decision. Uncertain deliveries retain the
// original recipient, payload and idempotency key; it cannot create a fresh send.
func (c *Continuations) Resume(ctx context.Context, storyID, recoveryID string, revision int64) (domain.StoryContinuationStatus, error) {
	if !validID(recoveryID) {
		return domain.StoryContinuationStatus{}, invalid("A stable recovery id is required")
	}
	story, err := c.Store.GetStory(ctx, storyID)
	if err != nil {
		return domain.StoryContinuationStatus{}, err
	}
	rec, found, err := c.Store.GetStoryContinuation(ctx, storyID)
	if err != nil {
		return domain.StoryContinuationStatus{}, err
	}
	if found && rec.RecoveryID == recoveryID {
		return c.Status(ctx, storyID)
	}
	if !found || rec.Revision != revision || rec.State != "blocked" || !story.Autonomous || story.Status == "done" || story.Question != "" {
		return domain.StoryContinuationStatus{}, invalid("Refresh the story and resolve its question before retrying blocked automation")
	}
	rec.RecoveryID = recoveryID
	rec.Generation++ // New dispatches must never reuse a completed generation after recovery.
	rec.Reason = ""
	rec.LeaseUntil = time.Time{}
	rec.NextAt = time.Time{}
	rec.WaitingSince = time.Time{}
	rec.DeliveryAttempts = 0
	rec.ProgressAttempts = 0
	retiredTurn := ""
	if rec.TurnID != "" {
		turn, err := c.Store.TurnByID(ctx, rec.TurnID)
		if err == nil && (turn.State == domain.TurnStateInterrupted || turn.State == domain.TurnStateCancelled) {
			retiredTurn = rec.TurnID
			rec.TurnID = ""
			rec.DeliveryID = ""
			rec.Text = ""
		}
	}
	// A known turn is inspected first. With no receipt, retry only the old key.
	switch {
	case rec.TurnID != "":
		rec.State = "waiting"
	case rec.DeliveryID != "":
		rec.State = "sending"
	default:
		rec.State = "ready"
	}
	audit := domain.StoryEvent{ID: fmt.Sprintf("automation-recovery-%x", sha256.Sum256([]byte(storyID+"\x00"+recoveryID))), StoryID: storyID, Kind: "automation_recovery", RecipientSessionID: rec.RecipientSessionID, Reference: retiredTurn, Text: "Explicit bounded automation retry authorized; unresolved delivery remains protected.", Result: "authorized", CreatedAt: c.now()}
	if retiredTurn != "" {
		audit.Text = "Explicit bounded automation retry authorized after the stopped turn; its original receipt is retained in history."
	}
	saved, err := c.Store.ResumeStoryContinuation(ctx, rec, story.Revision, audit)
	if err != nil {
		return domain.StoryContinuationStatus{}, err
	}
	if !saved {
		return domain.StoryContinuationStatus{}, invalid("Automation changed; refresh before retrying")
	}
	return c.Status(ctx, storyID)
}

// Exhaustion creates one explicit human decision, not repeated chat dialogue.
// Its stable key is the revision that entered blocked; retrying notification
// after a process crash cannot duplicate the conversation message.
func (c *Continuations) notifyBlocked(ctx context.Context, s domain.Story, rec domain.StoryContinuation) error {
	recorder, ok := c.Sender.(interface {
		RecordHumanDecision(context.Context, domain.SessionID, chatsvc.HumanDecision) (string, error)
	})
	if !ok {
		return nil
	}
	cfg, err := c.Store.GetCoordination(ctx)
	if err != nil {
		return err
	}
	if cfg.CoordinatorProjectID == "" {
		return nil
	}
	sessions, err := c.Store.ListSessions(ctx, cfg.CoordinatorProjectID)
	if err != nil {
		return err
	}
	var coordinator domain.SessionRecord
	for _, session := range sessions {
		if session.Kind == domain.KindOrchestrator && !session.IsTerminated && session.Activity.State != domain.ActivityExited && (coordinator.ID == "" || session.CreatedAt.After(coordinator.CreatedAt)) {
			coordinator = session
		}
	}
	if coordinator.ID == "" {
		return nil
	} // The same actionable reason remains on the board.
	_, err = recorder.RecordHumanDecision(ctx, coordinator.ID, chatsvc.HumanDecision{StoryID: s.ID, ID: fmt.Sprintf("story-blocked-%x-%d", sha256.Sum256([]byte(s.ID)), rec.Revision), Purpose: "blocking_decision", Question: "How should “" + s.Title + "” continue? Resolve the blocker, then confirm a bounded retry in the story, or change its scope/assignment.", WhyBlocked: publicContinuationReason(rec)})
	return err
}

// Older persisted reasons may contain internal handles. They are never public.
func publicContinuationReason(rec domain.StoryContinuation) string {
	reason := rec.Reason
	if rec.DeliveryID != "" {
		reason = strings.ReplaceAll(reason, rec.DeliveryID, "[retained delivery]")
	}
	return reason
}
