package autoresume

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	sessionmanager "github.com/aoagents/agent-orchestrator/backend/internal/session_manager"
)

var t0 = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

// world is a fake for every collaborator. It implements ONLY the narrow interfaces,
// which is the point of G2: the resumer compiles against nothing that can kill,
// clean up, replace or tear down.
type world struct {
	mu      sync.Mutex
	now     time.Time
	intents map[domain.SessionID]domain.ResumeIntent
	// sessions the store knows
	sessions map[domain.SessionID]domain.SessionRecord
	allowed  map[domain.SessionID]bool // missing = allowed
	stopped  map[domain.SessionID]domain.StopReason
	live     map[domain.SessionID]bool
	stories  []domain.Story
	coord    domain.ProjectID
	starts   int // daemon starts in the flap window

	// scripted behaviour
	resumeErrs map[domain.SessionID][]error // consumed in order; empty = success
	proof      map[domain.SessionID]sessionmanager.NativeHistoryProof
	parkOK     map[domain.SessionID]bool

	// recordings
	resumeCalls   []domain.SessionID
	parkCalls     []domain.SessionID
	notices       []string
	continues     []domain.SessionID
	sleeps        []time.Duration
	inFlight, max int
	probesIn, pmx int
	onResume      func(domain.SessionID)
	onProbe       func()
}

func newWorld() *world {
	return &world{
		now: t0, intents: map[domain.SessionID]domain.ResumeIntent{}, sessions: map[domain.SessionID]domain.SessionRecord{},
		allowed: map[domain.SessionID]bool{}, stopped: map[domain.SessionID]domain.StopReason{}, live: map[domain.SessionID]bool{},
		resumeErrs: map[domain.SessionID][]error{}, proof: map[domain.SessionID]sessionmanager.NativeHistoryProof{},
		parkOK: map[domain.SessionID]bool{}, coord: "coord",
	}
}

// add seeds one captured session with its intent.
func (w *world) add(id domain.SessionID, kind domain.SessionKind, project domain.ProjectID, class domain.ResumeClass, prior domain.ActivityState, capturedAfter time.Duration, turns ...domain.InterruptedTurn) {
	w.sessions[id] = domain.SessionRecord{ID: id, Kind: kind, ProjectID: project, Mode: domain.SessionModeChat}
	w.intents[id] = domain.ResumeIntent{
		SessionID: id, BootID: "boot-1", CapturedAt: t0.Add(capturedAfter), Class: class, PriorActivity: prior,
		InterruptedTurns: turns, State: domain.ResumePending, Revision: 1,
	}
}

func (w *world) ResumeAgentStrict(ctx context.Context, id domain.SessionID) (sessionmanager.RestoreResult, error) {
	w.mu.Lock()
	w.resumeCalls = append(w.resumeCalls, id)
	w.inFlight++
	w.max = max(w.max, w.inFlight)
	var err error
	if errs := w.resumeErrs[id]; len(errs) > 0 {
		err, w.resumeErrs[id] = errs[0], errs[1:]
	}
	hook := w.onResume
	w.mu.Unlock()
	if hook != nil {
		hook(id)
	}
	w.mu.Lock()
	w.inFlight--
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	if err == nil {
		w.live[id] = true
	}
	w.mu.Unlock()
	return sessionmanager.RestoreResult{}, err
}

func (w *world) ProbeNativeHistory(_ context.Context, id domain.SessionID) sessionmanager.NativeHistoryProof {
	w.mu.Lock()
	w.probesIn++
	w.pmx = max(w.pmx, w.probesIn)
	proof := w.proof[id]
	hook := w.onProbe
	w.mu.Unlock()
	if hook != nil {
		hook()
	}
	time.Sleep(time.Millisecond)
	w.mu.Lock()
	w.probesIn--
	w.mu.Unlock()
	return proof
}

func (w *world) ParkResumeSession(_ context.Context, id domain.SessionID) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.parkCalls = append(w.parkCalls, id)
	if w.parkOK[id] {
		rec := w.sessions[id]
		at := w.now
		rec.HibernatedAt = &at
		w.sessions[id] = rec
		return true, nil
	}
	return false, nil
}

func (w *world) HasLiveChatController(id domain.SessionID) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.live[id]
}

func (w *world) ListResumeIntents(context.Context) ([]domain.ResumeIntent, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]domain.ResumeIntent, 0, len(w.intents))
	for _, i := range w.intents {
		out = append(out, i)
	}
	slices.SortFunc(out, func(a, b domain.ResumeIntent) int { return int(a.SessionID[0]) - int(b.SessionID[0]) })
	return out, nil
}

func (w *world) SaveResumeIntent(ctx context.Context, intent domain.ResumeIntent) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err // like the real store: a cancelled context fails the write
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if cur, ok := w.intents[intent.SessionID]; !ok || cur.Revision != intent.Revision {
		return false, nil
	}
	intent.Revision++
	w.intents[intent.SessionID] = intent
	return true, nil
}

func (w *world) GetSession(_ context.Context, id domain.SessionID) (domain.SessionRecord, bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	rec, ok := w.sessions[id]
	return rec, ok, nil
}

func (w *world) AutoResumeAllowed(_ context.Context, id domain.SessionID) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	allowed, set := w.allowed[id]
	return !set || allowed
}

func (w *world) StopReason(_ context.Context, id domain.SessionID) (domain.StopReason, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stopped[id], nil
}

func (w *world) DaemonRunsSince(context.Context, time.Time) (int, error) { return w.starts, nil }

func (w *world) GetCoordination(context.Context) (domain.CoordinationConfig, error) {
	return domain.CoordinationConfig{CoordinatorProjectID: w.coord}, nil
}

func (w *world) ListStories(context.Context) ([]domain.Story, error) { return w.stories, nil }

func (w *world) NotifyResumeFailure(_ context.Context, id domain.SessionID, reason, bootID string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.notices = append(w.notices, fmt.Sprintf("%s|%s|%s", id, reason, bootID))
	return nil
}

func (w *world) SendContinue(_ context.Context, id domain.SessionID, _ string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.continues = append(w.continues, id)
	return nil
}

func (w *world) clock() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.now
}

// sleep advances the fake clock instead of waiting.
func (w *world) sleep(_ context.Context, d time.Duration) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.now = w.now.Add(d)
	w.sleeps = append(w.sleeps, d)
	return nil
}

func (w *world) resumer(cfg Config) *Resumer {
	cfg.Now, cfg.Sleep = w.clock, w.sleep
	return New("boot-1", w, w, w, w, w, slog.New(slog.NewTextHandler(io.Discard, nil)), cfg)
}

func (w *world) state(id domain.SessionID) domain.ResumeState { return w.intents[id].State }

func TestStagesBringUpTheCoordinatorThenManagersThenWorkers(t *testing.T) {
	w := newWorld()
	w.add("worker-a", domain.KindWorker, "repo", domain.ResumeWorking, domain.ActivityActive, 0)
	w.add("manager-1", domain.KindWorker, "repo", domain.ResumeWorking, domain.ActivityActive, time.Second)
	w.add("repo-orch", domain.KindOrchestrator, "repo", domain.ResumeWorking, domain.ActivityIdle, 2*time.Second)
	w.add("coord-1", domain.KindOrchestrator, "coord", domain.ResumeWorking, domain.ActivityIdle, 3*time.Second)
	w.add("worker-b", domain.KindWorker, "repo", domain.ResumeWorking, domain.ActivityActive, 4*time.Second)
	w.stories = []domain.Story{{ID: "s1", Autonomous: true, Status: "in_progress", Assignments: []domain.StoryAssignment{{SessionID: "manager-1", Role: "manager"}}}}
	started := map[domain.SessionID]int{}
	w.onResume = func(id domain.SessionID) { started[id] = len(w.resumeCalls) }

	if err := w.resumer(Config{Concurrency: 1}).Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	pos := func(id domain.SessionID) int { return slices.Index(w.resumeCalls, id) }
	ordered := pos("coord-1") < pos("manager-1") && pos("coord-1") < pos("repo-orch") &&
		pos("manager-1") < pos("worker-a") && pos("repo-orch") < pos("worker-a") && pos("worker-a") < pos("worker-b")
	if !ordered {
		t.Fatalf("resume order = %v, want coordinator, then managers, then workers oldest first", w.resumeCalls)
	}
	for _, id := range []domain.SessionID{"worker-a", "manager-1", "repo-orch", "coord-1", "worker-b"} {
		if w.state(id) != domain.ResumeResumed {
			t.Fatalf("%s = %s, want resumed", id, w.state(id))
		}
	}
	if len(w.notices) != 0 {
		t.Fatalf("a clean resume produced notices: %v", w.notices)
	}
}

func TestAStageNeverStartsBeforeTheOneAboveHasSettled(t *testing.T) {
	w := newWorld()
	w.add("coord-1", domain.KindOrchestrator, "coord", domain.ResumeWorking, domain.ActivityIdle, 0)
	w.add("worker-a", domain.KindWorker, "repo", domain.ResumeWorking, domain.ActivityActive, time.Second)
	coordUp := false
	w.onResume = func(id domain.SessionID) {
		if id == "worker-a" && !w.live["coord-1"] {
			t.Error("a worker started before the coordinator was up")
		}
		coordUp = coordUp || id == "coord-1"
	}
	if err := w.resumer(Config{}).Run(context.Background()); err != nil || !coordUp {
		t.Fatalf("run: %v coordUp=%v", err, coordUp)
	}
}

func TestAStageResumesAtMostThreeAtOnce(t *testing.T) {
	w := newWorld()
	for i := range 9 {
		w.add(domain.SessionID(fmt.Sprintf("w%d", i)), domain.KindWorker, "repo", domain.ResumeWorking, domain.ActivityActive, time.Duration(i)*time.Second)
	}
	var wg sync.WaitGroup
	gate := make(chan struct{})
	w.onResume = func(domain.SessionID) { wg.Add(1); defer wg.Done(); <-gate }
	done := make(chan error)
	go func() { done <- w.resumer(Config{}).Run(context.Background()) }()
	time.Sleep(60 * time.Millisecond)
	w.mu.Lock()
	in := w.inFlight
	w.mu.Unlock()
	if in > 3 {
		t.Fatalf("%d resumes in flight, want at most 3", in)
	}
	close(gate)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if w.max > 3 {
		t.Fatalf("peak concurrency %d, want at most 3", w.max)
	}
	if len(w.resumeCalls) != 9 {
		t.Fatalf("resumed %d of 9", len(w.resumeCalls))
	}
}

func TestAnIntentThatNoLongerAppliesIsAbandonedWithoutResuming(t *testing.T) {
	cases := map[string]func(*world){
		"policy never":       func(w *world) { w.allowed["w"] = false },
		"stopped on purpose": func(w *world) { w.stopped["w"] = domain.StopReasonUser },
		"terminated":         func(w *world) { r := w.sessions["w"]; r.IsTerminated = true; w.sessions["w"] = r },
		"hibernated":         func(w *world) { r := w.sessions["w"]; at := t0; r.HibernatedAt = &at; w.sessions["w"] = r },
		"already running":    func(w *world) { w.live["w"] = true },
		"gone":               func(w *world) { delete(w.sessions, "w") },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			w := newWorld()
			w.add("w", domain.KindWorker, "repo", domain.ResumeWorking, domain.ActivityActive, 0)
			mutate(w)
			if err := w.resumer(Config{}).Run(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(w.resumeCalls) != 0 || len(w.notices) != 0 || w.state("w") != domain.ResumeAbandoned {
				t.Fatalf("calls=%v notices=%v state=%s; want abandoned silently with no resume", w.resumeCalls, w.notices, w.state("w"))
			}
		})
	}
}

func TestOutcomesOfAResumeAttempt(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantState  domain.ResumeState
		wantCalls  int
		wantNotice string
	}{
		{"already running counts as resumed", sessionmanager.ErrAgentNotExited, domain.ResumeResumed, 1, ""},
		{"sign-in needs a person: no retry", ports.ErrChatAuthRequired, domain.ResumeFailed, 1, string(sessionmanager.ResumeReasonSignIn)},
		{"unresumable history needs a person", sessionmanager.ErrNotResumable, domain.ResumeFailed, 1, string(sessionmanager.ResumeReasonHistory)},
		{"a missing provider session needs a person", ports.ErrChatResumeFailed, domain.ResumeFailed, 1, string(sessionmanager.ResumeReasonHistory)},
		{"no workspace is permanent", sessionmanager.ErrIncompleteHandle, domain.ResumeAbandoned, 1, ReasonWorktreeMissing},
		{"terminated is permanent", sessionmanager.ErrTerminated, domain.ResumeAbandoned, 1, string(sessionmanager.ResumeReasonUnresumable)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld()
			w.add("w", domain.KindWorker, "repo", domain.ResumeWorking, domain.ActivityActive, 0)
			w.resumeErrs["w"] = []error{fmt.Errorf("resume agent w: %w", tc.err)}
			if err := w.resumer(Config{}).Run(context.Background()); err != nil {
				t.Fatal(err)
			}
			if w.state("w") != tc.wantState || len(w.resumeCalls) != tc.wantCalls {
				t.Fatalf("state=%s calls=%d, want %s/%d", w.state("w"), len(w.resumeCalls), tc.wantState, tc.wantCalls)
			}
			if tc.wantNotice == "" {
				if len(w.notices) != 0 {
					t.Fatalf("unexpected notices %v", w.notices)
				}
				return
			}
			if want := "w|" + tc.wantNotice + "|boot-1"; len(w.notices) != 1 || w.notices[0] != want {
				t.Fatalf("notices = %v, want exactly %q", w.notices, want)
			}
		})
	}
}

func TestATransientFailureBacksOffThenSucceedsWithoutANotice(t *testing.T) {
	w := newWorld()
	w.add("w", domain.KindWorker, "repo", domain.ResumeWorking, domain.ActivityActive, 0)
	w.resumeErrs["w"] = []error{ports.ErrChatDriverUnavailable, sessionmanager.ErrResumeInProgress, context.DeadlineExceeded}
	if err := w.resumer(Config{}).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w.state("w") != domain.ResumeResumed || len(w.resumeCalls) != 4 || len(w.notices) != 0 {
		t.Fatalf("state=%s calls=%d notices=%v; want resumed on the 4th attempt, silent", w.state("w"), len(w.resumeCalls), w.notices)
	}
	// The waits between attempts are the backoff schedule 30 s, 1 min, 2 min (after the
	// 20 s settle delay and any start spacing).
	waits := slices.DeleteFunc(slices.Clone(w.sleeps), func(d time.Duration) bool { return d < 30*time.Second || d == 90*time.Second })
	if want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute}; !slices.Equal(waits, want) {
		t.Fatalf("backoff waits = %v, want %v", waits, want)
	}
}

func TestTransientAndUnknownFailuresStopAfterFiveAttemptsWithOneNotice(t *testing.T) {
	for name, failure := range map[string]error{"transient": ports.ErrChatDriverUnavailable, "unknown": errors.New("boom")} {
		t.Run(name, func(t *testing.T) {
			w := newWorld()
			w.add("w", domain.KindWorker, "repo", domain.ResumeWorking, domain.ActivityActive, 0)
			w.resumeErrs["w"] = []error{failure, failure, failure, failure, failure, failure, failure}
			if err := w.resumer(Config{}).Run(context.Background()); err != nil {
				t.Fatal(err)
			}
			if w.state("w") != domain.ResumeFailed || len(w.resumeCalls) != 5 || len(w.notices) != 1 {
				t.Fatalf("state=%s calls=%d notices=%v; want failed after exactly 5 attempts with one notice", w.state("w"), len(w.resumeCalls), w.notices)
			}
		})
	}
}

func TestTheFlapGuardRefusesTheFourthResumeInThirtyMinutes(t *testing.T) {
	w := newWorld()
	w.add("w", domain.KindWorker, "repo", domain.ResumeWorking, domain.ActivityActive, 0)
	in := w.intents["w"]
	in.RecentResumes = []time.Time{t0.Add(-25 * time.Minute), t0.Add(-15 * time.Minute), t0.Add(-5 * time.Minute)}
	w.intents["w"] = in
	if err := w.resumer(Config{}).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(w.resumeCalls) != 0 || w.state("w") != domain.ResumeFailed || len(w.notices) != 1 || w.notices[0] != "w|"+ReasonFlapping+"|boot-1" {
		t.Fatalf("calls=%v state=%s notices=%v; want refused as flapping with one notice", w.resumeCalls, w.state("w"), w.notices)
	}

	// Resumes older than the window do not count.
	w2 := newWorld()
	w2.add("w", domain.KindWorker, "repo", domain.ResumeWorking, domain.ActivityActive, 0)
	in = w2.intents["w"]
	in.RecentResumes = []time.Time{t0.Add(-3 * time.Hour), t0.Add(-2 * time.Hour), t0.Add(-time.Hour)}
	w2.intents["w"] = in
	if err := w2.resumer(Config{}).Run(context.Background()); err != nil || w2.state("w") != domain.ResumeResumed {
		t.Fatalf("old resumes counted against the guard: state=%s err=%v", w2.state("w"), err)
	}
	if got := len(w2.intents["w"].RecentResumes); got != 1 {
		t.Fatalf("recent resumes = %d, want the old ones dropped and the new one kept", got)
	}
}

func TestSettleDelayLengthensWhenTheDaemonKeepsRestarting(t *testing.T) {
	for starts, want := range map[int]time.Duration{1: 20 * time.Second, 3: 20 * time.Second, 4: 2 * time.Minute} {
		w := newWorld()
		w.starts = starts
		w.add("w", domain.KindWorker, "repo", domain.ResumeWorking, domain.ActivityActive, 0)
		if err := w.resumer(Config{}).Run(context.Background()); err != nil {
			t.Fatal(err)
		}
		if w.sleeps[0] != want {
			t.Fatalf("%d starts: settle = %v, want %v", starts, w.sleeps[0], want)
		}
	}
}

func TestAnIntentLeftResumingByACrashIsResumedAgain(t *testing.T) {
	w := newWorld()
	w.add("w", domain.KindWorker, "repo", domain.ResumeWorking, domain.ActivityActive, 0)
	in := w.intents["w"]
	in.State, in.Attempts = domain.ResumeResuming, 1
	w.intents["w"] = in
	// The earlier resume did finish: the strict resume reports the agent already up.
	w.resumeErrs["w"] = []error{sessionmanager.ErrAgentNotExited}
	if err := w.resumer(Config{}).Run(context.Background()); err != nil || w.state("w") != domain.ResumeResumed {
		t.Fatalf("state=%s err=%v; want resumed", w.state("w"), err)
	}
}

func TestIdleSessionsArePausedOnlyOnProofAndOtherwiseResumedActively(t *testing.T) {
	w := newWorld()
	w.add("proven", domain.KindWorker, "repo", domain.ResumeIdle, domain.ActivityIdle, 0)
	w.add("gone", domain.KindWorker, "repo", domain.ResumeIdle, domain.ActivityIdle, 0)
	w.add("noprobe", domain.KindWorker, "repo", domain.ResumeIdle, domain.ActivityIdle, 0)
	w.add("contended", domain.KindWorker, "repo", domain.ResumeIdle, domain.ActivityIdle, 0)
	w.proof["proven"], w.proof["gone"] = sessionmanager.NativeHistoryPresent, sessionmanager.NativeHistoryMissing
	w.proof["noprobe"] = sessionmanager.NativeHistoryUnproven
	w.proof["contended"] = sessionmanager.NativeHistoryPresent
	// Parking would succeed for these, so wrongly parking an unproven session shows.
	w.parkOK["proven"], w.parkOK["noprobe"] = true, true // contended never parks

	if err := w.resumer(Config{}).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w.state("proven") != domain.ResumeHibernated || slices.Contains(w.resumeCalls, "proven") {
		t.Fatalf("a session with proven history must be parked, not launched: state=%s calls=%v", w.state("proven"), w.resumeCalls)
	}
	if w.state("gone") != domain.ResumeFailed || slices.Contains(w.resumeCalls, "gone") || slices.Contains(w.parkCalls, "gone") {
		t.Fatalf("a missing conversation must fail, never park or launch: state=%s", w.state("gone"))
	}
	if want := "gone|" + string(sessionmanager.ResumeReasonHistory) + "|boot-1"; !slices.Contains(w.notices, want) {
		t.Fatalf("notices = %v, want %q", w.notices, want)
	}
	if slices.Contains(w.parkCalls, "noprobe") {
		t.Fatal("a session with unproven history was parked")
	}
	for _, id := range []domain.SessionID{"noprobe", "contended"} {
		if w.state(id) != domain.ResumeResumed || !slices.Contains(w.resumeCalls, id) {
			t.Fatalf("%s: unprovable history must resume actively (strict), state=%s calls=%v", id, w.state(id), w.resumeCalls)
		}
	}
}

func TestThirtyIdleSessionsAreProbedThreeAtATime(t *testing.T) {
	w := newWorld()
	for i := range 30 {
		id := domain.SessionID(fmt.Sprintf("i%02d", i))
		w.add(id, domain.KindWorker, "repo", domain.ResumeIdle, domain.ActivityIdle, time.Duration(i)*time.Second)
		w.proof[id] = sessionmanager.NativeHistoryPresent
		w.parkOK[id] = true
	}
	if err := w.resumer(Config{}).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w.pmx > 3 || w.pmx == 0 {
		t.Fatalf("peak probes in flight = %d, want 1..3", w.pmx)
	}
	if len(w.parkCalls) != 30 || len(w.resumeCalls) != 0 {
		t.Fatalf("parked %d, resumed %d; want all 30 parked and none launched", len(w.parkCalls), len(w.resumeCalls))
	}
}

func TestContinueTurnsGoOutAfterEverySessionIsUpOnceAndOnlyWhereWarranted(t *testing.T) {
	w := newWorld()
	w.add("coord-1", domain.KindOrchestrator, "coord", domain.ResumeWorking, domain.ActivityActive, 0)
	w.add("delivered", domain.KindWorker, "repo", domain.ResumeWorking, domain.ActivityIdle, time.Second, domain.InterruptedTurn{ID: "t1", Delivered: true})
	w.add("undelivered", domain.KindWorker, "repo", domain.ResumeWorking, domain.ActivityIdle, 2*time.Second, domain.InterruptedTurn{ID: "t2", Delivered: false})
	w.add("was-active", domain.KindWorker, "repo", domain.ResumeWorking, domain.ActivityActive, 3*time.Second)
	w.add("quiet", domain.KindWorker, "repo", domain.ResumeWorking, domain.ActivityBlocked, 4*time.Second)
	w.add("failed", domain.KindWorker, "repo", domain.ResumeWorking, domain.ActivityActive, 5*time.Second)
	w.resumeErrs["failed"] = []error{ports.ErrChatAuthRequired}
	// The continue pass must see every working intent settled.
	w.onResume = func(domain.SessionID) {
		if len(w.continues) != 0 {
			t.Error("a continue turn was sent before every session was up")
		}
	}

	r := w.resumer(Config{})
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := []domain.SessionID{"coord-1", "delivered", "was-active"}; !slices.Equal(w.continues, want) {
		t.Fatalf("continue turns = %v, want %v (coordinator first; an undelivered prompt, a quiet session and a failed resume get none)", w.continues, want)
	}
	// A second pass (a second sweep, a second boot of the same intent) sends nothing more.
	if err := w.resumer(Config{}).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(w.continues) != 3 {
		t.Fatalf("a repeat pass re-sent continue turns: %v", w.continues)
	}
}

func TestRunningTwiceNeverResumesTwice(t *testing.T) {
	w := newWorld()
	w.add("w", domain.KindWorker, "repo", domain.ResumeWorking, domain.ActivityIdle, 0)
	for range 2 {
		if err := w.resumer(Config{}).Run(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(w.resumeCalls) != 1 {
		t.Fatalf("resume calls = %d, want 1 across two passes", len(w.resumeCalls))
	}
}

func TestACancelledPassStopsAndLeavesIntentsPending(t *testing.T) {
	w := newWorld()
	w.add("w", domain.KindWorker, "repo", domain.ResumeWorking, domain.ActivityActive, 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := w.resumer(Config{})
	r.cfg.Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	if err := r.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
	if w.state("w") != domain.ResumePending || len(w.resumeCalls) != 0 {
		t.Fatalf("a cancelled pass touched the intent: state=%s calls=%v", w.state("w"), w.resumeCalls)
	}
}

// G2: the resumer's whole reach into the session manager is these three methods.
// Adding Kill, Cleanup, RetireForReplacement, TeardownProject, Restore or any
// workspace call here is a visible, reviewable change that fails this test.
func TestTheResumerSessionCapabilityIsExactlyThreeMethods(t *testing.T) {
	typ := reflect.TypeOf((*Manager)(nil)).Elem()
	var names []string
	for i := range typ.NumMethod() {
		names = append(names, typ.Method(i).Name)
	}
	if want := []string{"ParkResumeSession", "ProbeNativeHistory", "ResumeAgentStrict"}; !slices.Equal(names, want) {
		t.Fatalf("Manager interface methods = %v, want exactly %v", names, want)
	}
}

// R1: the stage cap stops STARTING resumes. It must not cancel one in flight, and every
// write must survive cancellation, so no intent is left "resuming" with nothing to
// pick it up. The store here fails writes on a cancelled context, like the real one.
func TestTheStageCapNeverStrandsAnIntentResuming(t *testing.T) {
	w := newWorld()
	for i := range 4 {
		w.add(domain.SessionID(fmt.Sprintf("w%d", i)), domain.KindWorker, "repo", domain.ResumeWorking, domain.ActivityActive, time.Duration(i)*time.Second)
	}
	w.onResume = func(domain.SessionID) { time.Sleep(60 * time.Millisecond) } // outlasts the cap
	r := w.resumer(Config{StageCap: 30 * time.Millisecond, Concurrency: 1})
	r.cfg.Sleep = func(ctx context.Context, d time.Duration) error { // real time so the cap can fire
		w.mu.Lock()
		w.now = w.now.Add(d)
		w.mu.Unlock()
		time.Sleep(time.Millisecond)
		return ctx.Err()
	}
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	for id, in := range w.intents {
		if in.State == domain.ResumeResuming || in.State == domain.ResumePending {
			t.Fatalf("%s ended %s, want resumed (never stuck resuming or left pending)", id, in.State)
		}
	}
	for id, calls := range map[domain.SessionID]int{"w0": 1, "w1": 1, "w2": 1, "w3": 1} {
		if got := strings.Count(strings.Join(sessionsOf(w.resumeCalls), ","), string(id)); got != calls {
			t.Fatalf("%s resumed %d times, want %d: %v", id, got, calls, w.resumeCalls)
		}
	}
}

func sessionsOf(ids []domain.SessionID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = string(id)
	}
	return out
}

// A shutdown that cancels the run while an attempt is in flight records the intent as
// pending with a retry time (the write is detached), never as resuming.
func TestACancelledAttemptIsRecordedPendingNotResuming(t *testing.T) {
	w := newWorld()
	w.add("w", domain.KindWorker, "repo", domain.ResumeWorking, domain.ActivityActive, 0)
	ctx, cancel := context.WithCancel(context.Background())
	w.onResume = func(domain.SessionID) { cancel() }
	r := w.resumer(Config{})
	_ = r.Run(ctx)
	in := w.intents["w"]
	if in.State != domain.ResumePending || in.NextAttemptAt == nil {
		t.Fatalf("intent = %+v, want pending with a retry time after the cancelled attempt", in)
	}
}

// R2: an intent captured by the startup retry loop after the pass began is acted on.
func TestAnIntentCapturedAfterTheFirstScanIsStillResumedAndParked(t *testing.T) {
	w := newWorld() // nothing captured yet
	waited := 0
	cfg := Config{WaitStartupChecks: func(context.Context) error {
		waited++
		// The retry loop's last attempt ends definitively and captures two sessions.
		w.mu.Lock()
		defer w.mu.Unlock()
		w.sessions["late-work"] = domain.SessionRecord{ID: "late-work", Kind: domain.KindWorker, ProjectID: "repo", Mode: domain.SessionModeChat}
		w.intents["late-work"] = domain.ResumeIntent{SessionID: "late-work", BootID: "boot-1", CapturedAt: t0, Class: domain.ResumeWorking, PriorActivity: domain.ActivityActive, State: domain.ResumePending, Revision: 1}
		w.sessions["late-idle"] = domain.SessionRecord{ID: "late-idle", Kind: domain.KindWorker, ProjectID: "repo", Mode: domain.SessionModeChat}
		w.intents["late-idle"] = domain.ResumeIntent{SessionID: "late-idle", BootID: "boot-1", CapturedAt: t0, Class: domain.ResumeIdle, PriorActivity: domain.ActivityIdle, State: domain.ResumePending, Revision: 1}
		w.proof["late-idle"], w.parkOK["late-idle"] = sessionmanager.NativeHistoryPresent, true
		return nil
	}}
	if err := w.resumer(cfg).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if waited != 1 {
		t.Fatalf("waited for the startup checks %d times, want once", waited)
	}
	if w.state("late-work") != domain.ResumeResumed || w.state("late-idle") != domain.ResumeHibernated {
		t.Fatalf("late-work=%s late-idle=%s; want resumed and parked", w.state("late-work"), w.state("late-idle"))
	}
	if len(w.resumeCalls) != 1 || len(w.continues) != 1 {
		t.Fatalf("resumes=%v continues=%v, want one of each (no duplicates)", w.resumeCalls, w.continues)
	}
	// A second pass over the same state does nothing more.
	if err := w.resumer(cfg).Run(context.Background()); err != nil || len(w.resumeCalls) != 1 || len(w.continues) != 1 {
		t.Fatalf("a repeat pass acted again: resumes=%v continues=%v err=%v", w.resumeCalls, w.continues, err)
	}
}

func TestAContinueTurnForAnOldIntentIsDropped(t *testing.T) {
	w := newWorld()
	w.add("w", domain.KindWorker, "repo", domain.ResumeWorking, domain.ActivityActive, 0)
	in := w.intents["w"]
	in.State, in.CapturedAt = domain.ResumeResumed, t0.Add(-2*time.Hour)
	w.intents["w"] = in
	w.live["w"] = true
	if err := w.resumer(Config{}).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(w.continues) != 0 {
		t.Fatalf("a continue turn went out for an intent captured two hours ago: %v", w.continues)
	}
}

func TestAProtectedRefusalFromTheManagerIsSilent(t *testing.T) {
	w := newWorld()
	w.add("w", domain.KindWorker, "repo", domain.ResumeWorking, domain.ActivityActive, 0)
	w.resumeErrs["w"] = []error{sessionmanager.ErrSessionProtected}
	if err := w.resumer(Config{}).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w.state("w") != domain.ResumeAbandoned || len(w.notices) != 0 {
		t.Fatalf("state=%s notices=%v; want abandoned with no notice", w.state("w"), w.notices)
	}
}

// P4: losing the compare-and-swap when promoting an unproven idle intent must not leave
// it idle and pending forever.
func TestLosingThePromotionRaceStillResumesTheSession(t *testing.T) {
	w := newWorld()
	w.add("w", domain.KindWorker, "repo", domain.ResumeIdle, domain.ActivityIdle, 0)
	w.proof["w"] = sessionmanager.NativeHistoryUnproven
	w.onProbe = func() { // someone else touches the intent between our read and our write
		w.mu.Lock()
		in := w.intents["w"]
		in.Revision++
		w.intents["w"] = in
		w.mu.Unlock()
	}
	if err := w.resumer(Config{}).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w.state("w") != domain.ResumeResumed {
		t.Fatalf("state=%s, want resumed after losing the promotion race", w.state("w"))
	}
}
