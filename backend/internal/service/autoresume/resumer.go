// Package autoresume brings chat sessions back after a reboot or an AO relaunch so
// agents never sit idle. It acts only on resume intents the boot captured, and only
// through the narrow Manager interface: it can resume an exited agent strictly (the
// same provider conversation or nothing), park an idle one asleep, and read state.
// It has no way to kill a session, clean up a worktree, replace an orchestrator or
// tear a project down, and it never starts a fresh conversation.
package autoresume

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	sessionmanager "github.com/aoagents/agent-orchestrator/backend/internal/session_manager"
)

// Manager is the ONLY session capability the resumer holds.
type Manager interface {
	ResumeAgentStrict(ctx context.Context, id domain.SessionID) (sessionmanager.RestoreResult, error)
	ProbeNativeHistory(ctx context.Context, id domain.SessionID) sessionmanager.NativeHistoryProof
	ParkResumeSession(ctx context.Context, id domain.SessionID) (bool, error)
}

// Chat answers whether a session already has a live controller.
type Chat interface {
	HasLiveChatController(id domain.SessionID) bool
}

// Store is the durable state the resumer reads and the intents it owns.
type Store interface {
	ListResumeIntents(ctx context.Context) ([]domain.ResumeIntent, error)
	SaveResumeIntent(ctx context.Context, intent domain.ResumeIntent) (bool, error)
	GetSession(ctx context.Context, id domain.SessionID) (domain.SessionRecord, bool, error)
	AutoResumeAllowed(ctx context.Context, id domain.SessionID) bool
	StopReason(ctx context.Context, id domain.SessionID) (domain.StopReason, error)
	DaemonRunsSince(ctx context.Context, since time.Time) (int, error)
	GetCoordination(ctx context.Context) (domain.CoordinationConfig, error)
	ListStories(ctx context.Context) ([]domain.Story, error)
}

// Notifier tells the human that a session could not come back. It is idempotent per
// session and boot; reason is a fixed phrase, never raw error text.
type Notifier interface {
	NotifyResumeFailure(ctx context.Context, id domain.SessionID, reason, bootID string) error
}

// Continuer sends the one keyed "continue" turn to a resumed session. It is
// idempotent by its client message id.
type Continuer interface {
	SendContinue(ctx context.Context, id domain.SessionID, bootID string) error
}

// Reasons the resumer itself reports, beside the fixed phrases of the session manager.
const (
	ReasonFlapping        = "it keeps stopping after restarts"
	ReasonWorktreeMissing = "its worktree is missing"
)

// Config tunes the pass. Zero values take the defaults.
type Config struct {
	// Settle is the delay before the first resume; SettleAfterFlap replaces it when
	// the daemon started more than FlapStarts times in FlapWindow.
	Settle, SettleAfterFlap time.Duration
	FlapStarts              int
	FlapWindow              time.Duration
	// StageCap bounds how long one stage may take before the next starts.
	StageCap time.Duration
	// Concurrency and StartSpacing bound how many sessions come up at once and how
	// fast they are started within a stage.
	Concurrency    int
	StartSpacing   time.Duration
	AttemptTimeout time.Duration
	// Backoff is the wait before each retry of a transient failure; MaxAttempts is the
	// number of attempts per intent.
	Backoff     []time.Duration
	MaxAttempts int
	// MaxResumes successful resumes of one session within ResumeWindow is the most
	// the flap guard allows.
	MaxResumes   int
	ResumeWindow time.Duration
	// ProbeConcurrency bounds the idle-session history probes in flight.
	ProbeConcurrency int
	// ContinueExpiry is how long after capture a continue turn is still worth
	// sending; a later one would tell an agent that may be idle or finished to
	// "continue where you left off".
	ContinueExpiry time.Duration
	// WaitStartupChecks blocks until the boot health pass and its background reattach
	// retries are done: a late retry can capture an intent minutes after boot. Nil
	// means there is nothing to wait for.
	WaitStartupChecks func(ctx context.Context) error
	// WriteTimeout bounds each state write, which runs detached from cancellation.
	WriteTimeout time.Duration
	Now          func() time.Time
	Sleep        func(ctx context.Context, d time.Duration) error
}

func (c Config) withDefaults() Config {
	setDuration := func(v *time.Duration, d time.Duration) {
		if *v == 0 {
			*v = d
		}
	}
	setInt := func(v *int, n int) {
		if *v == 0 {
			*v = n
		}
	}
	setDuration(&c.Settle, 20*time.Second)
	setDuration(&c.SettleAfterFlap, 2*time.Minute)
	setInt(&c.FlapStarts, 3)
	setDuration(&c.FlapWindow, 10*time.Minute)
	setDuration(&c.StageCap, 3*time.Minute)
	setInt(&c.Concurrency, 3)
	setDuration(&c.StartSpacing, 2*time.Second)
	setDuration(&c.AttemptTimeout, 90*time.Second)
	if len(c.Backoff) == 0 {
		c.Backoff = []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute}
	}
	setInt(&c.MaxAttempts, 5)
	setInt(&c.MaxResumes, 3)
	setDuration(&c.ResumeWindow, 30*time.Minute)
	setInt(&c.ProbeConcurrency, 3)
	setDuration(&c.ContinueExpiry, 15*time.Minute)
	setDuration(&c.WriteTimeout, 10*time.Second)
	if c.Now == nil {
		c.Now = func() time.Time { return time.Now().UTC() }
	}
	if c.Sleep == nil {
		c.Sleep = func(ctx context.Context, d time.Duration) error {
			timer := time.NewTimer(d)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		}
	}
	return c
}

// Resumer runs one auto-resume pass for one boot.
type Resumer struct {
	manager   Manager
	chat      Chat
	store     Store
	notify    Notifier
	continuer Continuer
	log       *slog.Logger
	cfg       Config
	bootID    string

	stagesOnce sync.Once
	managers   map[domain.SessionID]bool
	coord      domain.ProjectID
}

// New builds a resumer. continuer and notify may be nil in tests that do not need them.
func New(bootID string, manager Manager, chat Chat, store Store, notify Notifier, continuer Continuer, log *slog.Logger, cfg Config) *Resumer {
	if log == nil {
		log = slog.Default()
	}
	return &Resumer{manager: manager, chat: chat, store: store, notify: notify, continuer: continuer, log: log, cfg: cfg.withDefaults(), bootID: bootID}
}

// Run performs the pass: park idle sessions, wait for the system to settle, bring
// working sessions up in stages, then deliver the continue turns. It keeps scanning
// until the boot's startup checks are done, because a background reattach retry can
// capture an intent minutes after the first scan; late intents are parked or resumed
// the same way, without a second settle delay. It is safe to run again after a
// crash: intents carry their own progress.
func (r *Resumer) Run(ctx context.Context) error {
	intents, err := r.store.ListResumeIntents(ctx)
	if err != nil {
		return err
	}
	for _, intent := range intents {
		if intent.State == domain.ResumeResuming {
			// A daemon killed mid-resume left it. A resume that did finish reads as
			// already running below, which counts as resumed.
			intent.State = domain.ResumePending
			r.save(ctx, &intent)
		}
	}
	settled := r.cfg.WaitStartupChecks == nil
	settleDone := false
	// A round that finds nothing pending either ends the pass (startup checks are
	// done) or waits for them once and looks again. Bounded so a write that keeps
	// failing cannot spin.
	for round := 0; round < 20; round++ {
		pending := r.pendingIntents(ctx)
		if len(pending) == 0 {
			if settled {
				break
			}
			if err := r.cfg.WaitStartupChecks(ctx); err != nil {
				return err
			}
			settled = true
			continue
		}
		r.parkIdle(ctx, pending)
		if r.hasWorking(ctx) {
			if !settleDone {
				if err := r.cfg.Sleep(ctx, r.settleDelay(ctx)); err != nil {
					return err
				}
				settleDone = true
			}
			if err := r.resumeWorking(ctx); err != nil {
				return err
			}
		}
		if err := r.deliverContinues(ctx); err != nil {
			return err
		}
	}
	return r.deliverContinues(ctx)
}

func (r *Resumer) settleDelay(ctx context.Context) time.Duration {
	starts, err := r.store.DaemonRunsSince(ctx, r.cfg.Now().Add(-r.cfg.FlapWindow))
	if err == nil && starts > r.cfg.FlapStarts {
		return r.cfg.SettleAfterFlap
	}
	return r.cfg.Settle
}

// save writes an intent with its compare-and-swap revision and bumps the local copy.
//
// The write is detached from the caller's cancellation (and bounded): a stage cut-off
// or a shutdown that cancels an attempt must still be able to record where the
// intent stands, or it would stay "resuming" with nothing to pick it up again.
func (r *Resumer) save(ctx context.Context, intent *domain.ResumeIntent) bool {
	intent.UpdatedAt = r.cfg.Now()
	ctx, cancel := r.detached(ctx)
	defer cancel()
	ok, err := r.store.SaveResumeIntent(ctx, *intent)
	if err != nil {
		r.log.Warn("auto-resume: could not save intent", "sessionID", intent.SessionID, "error", err)
		return false
	}
	if ok {
		intent.Revision++
	}
	return ok
}

// detached is a bounded context that survives the caller's cancellation.
func (r *Resumer) detached(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), r.cfg.WriteTimeout)
}

// pendingIntents lists every intent still waiting for a first or later attempt,
// idle and working alike.
func (r *Resumer) pendingIntents(ctx context.Context) []domain.ResumeIntent {
	return append(r.liveIntents(ctx, domain.ResumeIdle), r.liveIntents(ctx, domain.ResumeWorking)...)
}

func (r *Resumer) liveIntents(ctx context.Context, class domain.ResumeClass) []domain.ResumeIntent {
	intents, err := r.store.ListResumeIntents(ctx)
	if err != nil {
		return nil
	}
	var out []domain.ResumeIntent
	for _, intent := range intents {
		if intent.State == domain.ResumePending && intent.Class == class {
			out = append(out, intent)
		}
	}
	slices.SortStableFunc(out, func(a, b domain.ResumeIntent) int { return a.CapturedAt.Compare(b.CapturedAt) })
	return out
}

func (r *Resumer) hasWorking(ctx context.Context) bool {
	return len(r.liveIntents(ctx, domain.ResumeWorking)) > 0
}

// obsolete reports why an intent no longer applies, or "" when the session still
// needs an unattended action. Policy fails closed, and a changed policy takes effect
// immediately.
func (r *Resumer) obsolete(ctx context.Context, id domain.SessionID) string {
	if !r.store.AutoResumeAllowed(ctx, id) {
		return "the policy is not allowed"
	}
	if reason, err := r.store.StopReason(ctx, id); err != nil || reason != domain.StopReasonNone {
		return "it was stopped on purpose"
	}
	rec, found, err := r.store.GetSession(ctx, id)
	switch {
	case err != nil:
		return "its record is unreadable"
	case !found || rec.IsTerminated:
		return "it is gone"
	case rec.HibernatedAt != nil:
		return "it is asleep"
	case r.chat.HasLiveChatController(id):
		return "it is already running"
	}
	return ""
}

// settle ends an intent in a terminal state.
func (r *Resumer) settle(ctx context.Context, intent *domain.ResumeIntent, state domain.ResumeState, note string) {
	intent.State, intent.LastError, intent.NextAttemptAt = state, note, nil
	r.save(ctx, intent)
}

// fail ends an intent and tells the human, once.
func (r *Resumer) fail(ctx context.Context, intent *domain.ResumeIntent, state domain.ResumeState, reason string) {
	r.settle(ctx, intent, state, reason)
	if r.notify == nil {
		return
	}
	noticeCtx, cancel := r.detached(ctx)
	defer cancel()
	if err := r.notify.NotifyResumeFailure(noticeCtx, intent.SessionID, reason, r.bootID); err != nil {
		r.log.Warn("auto-resume: could not record the failure notice", "sessionID", intent.SessionID, "error", err)
	}
}

// parkIdle puts idle sessions to sleep, at most ProbeConcurrency probes at a time.
func (r *Resumer) parkIdle(ctx context.Context, live []domain.ResumeIntent) {
	var idle []domain.ResumeIntent
	for _, intent := range live {
		if intent.Class == domain.ResumeIdle && intent.State == domain.ResumePending {
			idle = append(idle, intent)
		}
	}
	slots := make(chan struct{}, r.cfg.ProbeConcurrency)
	var wg sync.WaitGroup
	for i := range idle {
		intent := idle[i]
		slots <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			r.parkOne(ctx, intent)
		}()
	}
	wg.Wait()
}

// parkOne parks one idle session once its saved conversation is proven present.
// Anything short of proof resumes it actively instead, which fails safe: a missing
// conversation ends in a notice, never in a fresh start.
func (r *Resumer) parkOne(ctx context.Context, intent domain.ResumeIntent) {
	if why := r.obsolete(ctx, intent.SessionID); why != "" {
		r.settle(ctx, &intent, domain.ResumeAbandoned, why)
		return
	}
	switch r.manager.ProbeNativeHistory(ctx, intent.SessionID) {
	case sessionmanager.NativeHistoryMissing:
		r.fail(ctx, &intent, domain.ResumeFailed, string(sessionmanager.ResumeReasonHistory))
		return
	case sessionmanager.NativeHistoryPresent:
		for range 3 {
			parked, err := r.manager.ParkResumeSession(ctx, intent.SessionID)
			if err != nil {
				r.log.Warn("auto-resume: park failed", "sessionID", intent.SessionID, "error", err)
				break
			}
			if parked {
				r.settle(ctx, &intent, domain.ResumeHibernated, "")
				return
			}
			if why := r.obsolete(ctx, intent.SessionID); why != "" {
				r.settle(ctx, &intent, domain.ResumeAbandoned, why)
				return
			}
		}
	}
	// Unproven, or contention that never cleared: resume it actively.
	intent.Class = domain.ResumeWorking
	if !r.save(ctx, &intent) {
		// Another writer changed it first: look again, and promote the fresh copy if
		// it is still waiting, so the intent never stays idle and pending forever.
		current, ok := r.find(ctx, intent.SessionID)
		if ok && current.State == domain.ResumePending && current.Class == domain.ResumeIdle {
			current.Class = domain.ResumeWorking
			r.save(ctx, &current)
		}
	}
}

func (r *Resumer) find(ctx context.Context, id domain.SessionID) (domain.ResumeIntent, bool) {
	intents, err := r.store.ListResumeIntents(ctx)
	if err != nil {
		return domain.ResumeIntent{}, false
	}
	for _, intent := range intents {
		if intent.SessionID == id {
			return intent, true
		}
	}
	return domain.ResumeIntent{}, false
}

// stageOf orders sessions: 1 the main coordinator, 2 repo and story managers, 3 the
// rest. A session comes up only after the stage above it has settled, so a resumed
// coordinator never messages a manager that is not up yet.
func (r *Resumer) stageOf(ctx context.Context, id domain.SessionID) int {
	r.stagesOnce.Do(func() {
		r.managers = map[domain.SessionID]bool{}
		if cfg, err := r.store.GetCoordination(ctx); err == nil {
			r.coord = cfg.CoordinatorProjectID
		}
		if stories, err := r.store.ListStories(ctx); err == nil {
			for _, story := range stories {
				if !story.Autonomous || story.Status == "done" {
					continue
				}
				for _, a := range story.Assignments {
					if a.Role == "manager" {
						r.managers[a.SessionID] = true
					}
				}
			}
		}
	})
	rec, found, err := r.store.GetSession(ctx, id)
	switch {
	case err != nil || !found:
		return 3
	case rec.Kind == domain.KindOrchestrator && r.coord != "" && rec.ProjectID == r.coord:
		return 1
	case rec.Kind == domain.KindOrchestrator, r.managers[id]:
		return 2
	}
	return 3
}

// resumeWorking runs rounds until no working intent is left pending. Each round
// brings up what is due, stage by stage; a transient failure goes back to pending
// with a later attempt time and is picked up by a later round. Attempts are bounded,
// so the loop ends.
func (r *Resumer) resumeWorking(ctx context.Context) error {
	for {
		pending := r.liveIntents(ctx, domain.ResumeWorking)
		if len(pending) == 0 {
			return nil
		}
		now := r.cfg.Now()
		var due []domain.ResumeIntent
		var next time.Time
		for _, intent := range pending {
			if intent.NextAttemptAt == nil || !intent.NextAttemptAt.After(now) {
				due = append(due, intent)
			} else if next.IsZero() || intent.NextAttemptAt.Before(next) {
				next = *intent.NextAttemptAt
			}
		}
		if len(due) == 0 {
			if err := r.cfg.Sleep(ctx, next.Sub(now)); err != nil {
				return err
			}
			continue
		}
		byStage := map[int][]domain.ResumeIntent{}
		for _, intent := range due {
			stage := r.stageOf(ctx, intent.SessionID)
			byStage[stage] = append(byStage[stage], intent)
		}
		for stage := 1; stage <= 3; stage++ {
			if err := r.runStage(ctx, byStage[stage]); err != nil {
				return err
			}
		}
	}
}

// runStage resumes one stage: bounded concurrency, spaced starts, a hard cap. What
// the cap cuts off stays pending for the next round.
func (r *Resumer) runStage(ctx context.Context, intents []domain.ResumeIntent) error {
	if len(intents) == 0 {
		return nil
	}
	// The cap decides when to stop STARTING resumes; it never cancels one in flight
	// (each attempt has its own timeout), so a cut-off session is not burned an
	// attempt or left half-recorded. What it stops starting stays pending.
	stageCtx, cancel := context.WithTimeout(ctx, r.cfg.StageCap)
	defer cancel()
	slots := make(chan struct{}, r.cfg.Concurrency)
	var wg sync.WaitGroup
	for i, intent := range intents {
		if i > 0 {
			if err := r.cfg.Sleep(stageCtx, r.cfg.StartSpacing); err != nil {
				break
			}
		}
		select {
		case slots <- struct{}{}:
		case <-stageCtx.Done():
		}
		if stageCtx.Err() != nil {
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			r.resumeOne(ctx, intent)
		}()
	}
	wg.Wait()
	return ctx.Err()
}

func (r *Resumer) resumeOne(ctx context.Context, intent domain.ResumeIntent) {
	if why := r.obsolete(ctx, intent.SessionID); why != "" {
		r.settle(ctx, &intent, domain.ResumeAbandoned, why)
		return
	}
	now := r.cfg.Now()
	recent := r.recentResumes(intent, now)
	if len(recent) >= r.cfg.MaxResumes {
		r.fail(ctx, &intent, domain.ResumeFailed, ReasonFlapping)
		return
	}
	intent.State = domain.ResumeResuming
	intent.Attempts++
	intent.NextAttemptAt = nil
	if !r.save(ctx, &intent) {
		return // another writer owns it now
	}
	attemptCtx, cancel := context.WithTimeout(ctx, r.cfg.AttemptTimeout)
	_, err := r.manager.ResumeAgentStrict(attemptCtx, intent.SessionID)
	cancel()
	r.finishAttempt(ctx, &intent, err, recent)
}

func (r *Resumer) recentResumes(intent domain.ResumeIntent, now time.Time) []time.Time {
	var recent []time.Time
	for _, at := range intent.RecentResumes {
		if now.Sub(at) < r.cfg.ResumeWindow {
			recent = append(recent, at)
		}
	}
	return recent
}

func (r *Resumer) finishAttempt(ctx context.Context, intent *domain.ResumeIntent, err error, recent []time.Time) {
	now := r.cfg.Now()
	if err == nil {
		intent.RecentResumes = append(slices.Clone(recent), now)
		r.settle(ctx, intent, domain.ResumeResumed, "")
		return
	}
	switch sessionmanager.ClassifyResumeFailure(err) {
	case sessionmanager.ResumeFailureAlreadyRunning:
		intent.RecentResumes = recent
		r.settle(ctx, intent, domain.ResumeResumed, "")
	case sessionmanager.ResumeFailureRefused:
		r.settle(ctx, intent, domain.ResumeAbandoned, "refused")
	case sessionmanager.ResumeFailureNeedsPerson:
		r.fail(ctx, intent, domain.ResumeFailed, string(sessionmanager.ResumeFailureReason(err)))
	case sessionmanager.ResumeFailurePermanent:
		reason := string(sessionmanager.ResumeReasonUnresumable)
		if errors.Is(err, sessionmanager.ErrIncompleteHandle) || errors.Is(err, sessionmanager.ErrWorkspaceMissing) {
			reason = ReasonWorktreeMissing
		}
		r.fail(ctx, intent, domain.ResumeAbandoned, reason)
	default: // transient, and unknown errors until the attempt cap
		if intent.Attempts >= r.cfg.MaxAttempts {
			r.fail(ctx, intent, domain.ResumeFailed, string(sessionmanager.ResumeReasonUnresumable))
			return
		}
		wait := r.cfg.Backoff[min(intent.Attempts-1, len(r.cfg.Backoff)-1)]
		at := now.Add(wait)
		intent.State, intent.NextAttemptAt, intent.LastError = domain.ResumePending, &at, "will retry"
		r.save(ctx, intent)
	}
}

// needsContinue: a resumed session that was mid-turn (a human prompt the provider
// confirmed was cut off) or active gets exactly one "continue" turn. Prompts the
// provider never confirmed are not re-sent.
func (r *Resumer) needsContinue(intent domain.ResumeIntent) bool {
	if intent.State != domain.ResumeResumed || intent.ContinueSentAt != nil {
		return false
	}
	if r.cfg.Now().Sub(intent.CapturedAt) > r.cfg.ContinueExpiry {
		return false // too old: the agent may be idle or finished by now
	}
	return intent.PriorActivity == domain.ActivityActive ||
		slices.ContainsFunc(intent.InterruptedTurns, func(t domain.InterruptedTurn) bool { return t.Delivered })
}

// deliverContinues runs after every working intent has settled, in stage order, so a
// continue turn never reaches a session that is not up yet.
func (r *Resumer) deliverContinues(ctx context.Context) error {
	if r.continuer == nil {
		return nil
	}
	intents, err := r.store.ListResumeIntents(ctx)
	if err != nil {
		return err
	}
	byStage := map[int][]domain.ResumeIntent{}
	for _, intent := range intents {
		if r.needsContinue(intent) {
			stage := r.stageOf(ctx, intent.SessionID)
			byStage[stage] = append(byStage[stage], intent)
		}
	}
	for stage := 1; stage <= 3; stage++ {
		for _, intent := range byStage[stage] {
			if err := ctx.Err(); err != nil {
				return err
			}
			if !r.chat.HasLiveChatController(intent.SessionID) || !r.store.AutoResumeAllowed(ctx, intent.SessionID) {
				continue
			}
			if err := r.continuer.SendContinue(ctx, intent.SessionID, intent.BootID); err != nil {
				r.log.Warn("auto-resume: continue turn not sent", "sessionID", intent.SessionID, "error", err)
				continue
			}
			sent := r.cfg.Now()
			intent.ContinueSentAt = &sent
			r.save(ctx, &intent)
		}
	}
	return nil
}
