package reproof

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

type fakeManager struct {
	mu        sync.Mutex
	relaunch  []domain.SessionID
	err       map[domain.SessionID]error
	resumeErr map[domain.SessionID]error // ResumeStopped failures
	resumed   []domain.SessionID
	asked     map[domain.SessionID]int // RelaunchForIdentity calls per session
	noop      map[domain.SessionID]bool
	proofsFor func(domain.SessionID) // called on a successful relaunch (the new launch issues a proof)
}

func (f *fakeManager) RelaunchForIdentity(_ context.Context, id domain.SessionID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.asked == nil {
		f.asked = map[domain.SessionID]int{}
	}
	f.asked[id]++
	if err := f.err[id]; err != nil {
		return false, err
	}
	if f.noop[id] {
		return false, nil
	}
	f.relaunch = append(f.relaunch, id)
	if f.proofsFor != nil {
		f.proofsFor(id)
	}
	return true, nil
}

func (f *fakeManager) ResumeStopped(_ context.Context, id domain.SessionID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.resumeErr[id]; err != nil {
		return false, err
	}
	f.resumed = append(f.resumed, id)
	return true, nil
}

type fakeStopped struct {
	recorded []domain.SessionID
	notified []domain.SessionID
	reasons  []string
}

func (f *fakeStopped) RecordStopped(_ context.Context, id domain.SessionID) error {
	f.recorded = append(f.recorded, id)
	return nil
}

func (f *fakeStopped) NotifyGaveUp(_ context.Context, id domain.SessionID, reason string) error {
	f.notified = append(f.notified, id)
	f.reasons = append(f.reasons, reason)
	return nil
}

type fakeStore struct {
	sessions []domain.SessionRecord
	proofs   map[domain.SessionID]bool
	never    map[domain.SessionID]bool
	coordPrj domain.ProjectID
}

func (f *fakeStore) ListAllSessions(context.Context) ([]domain.SessionRecord, error) {
	return f.sessions, nil
}
func (f *fakeStore) SessionsWithProof(_ context.Context, ids []domain.SessionID) (map[domain.SessionID]bool, error) {
	out := map[domain.SessionID]bool{}
	for _, id := range ids {
		if f.proofs[id] {
			out[id] = true
		}
	}
	return out, nil
}
func (f *fakeStore) AutoResumeAllowed(_ context.Context, id domain.SessionID) bool {
	return !f.never[id]
}
func (f *fakeStore) GetCoordination(context.Context) (domain.CoordinationConfig, error) {
	return domain.CoordinationConfig{CoordinatorProjectID: f.coordPrj}, nil
}

var epoch = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }

func rec(id string, kind domain.SessionKind, project string, age int) domain.SessionRecord {
	return domain.SessionRecord{ID: domain.SessionID(id), ProjectID: domain.ProjectID(project), Kind: kind,
		Activity: domain.Activity{State: domain.ActivityIdle}, CreatedAt: epoch.Add(time.Duration(age) * time.Minute)}
}

func fixture() (*Service, *fakeManager, *fakeStore, *clock) {
	store := &fakeStore{
		coordPrj: "overall",
		proofs:   map[domain.SessionID]bool{},
		never:    map[domain.SessionID]bool{},
		sessions: []domain.SessionRecord{
			rec("worker-a", domain.KindWorker, "repo", 1),
			rec("manager-a", domain.KindOrchestrator, "repo", 2),
			rec("coord", domain.KindOrchestrator, "overall", 3),
			rec("worker-b", domain.KindWorker, "repo2", 4),
			rec("manager-b", domain.KindOrchestrator, "repo2", 5),
		},
	}
	mgr := &fakeManager{err: map[domain.SessionID]error{}, resumeErr: map[domain.SessionID]error{}, noop: map[domain.SessionID]bool{}}
	mgr.proofsFor = func(id domain.SessionID) { store.proofs[id] = true }
	clk := &clock{now: epoch.Add(time.Hour)}
	svc := New(mgr, store, &fakeStopped{}, slog.New(slog.NewTextHandler(io.Discard, nil)), Config{Now: clk.Now, StartupDelay: -1, ProjectCooldown: time.Millisecond})
	return svc, mgr, store, clk
}

func tick(t *testing.T, s *Service, clk *clock) {
	t.Helper()
	s.Tick(context.Background())
	clk.now = clk.now.Add(s.cfg.Every + time.Second)
}

// One relaunch per tick: the overall coordinator, then project managers, then workers.
func TestRelaunchOrderIsCoordinatorThenManagersThenWorkers(t *testing.T) {
	s, mgr, _, clk := fixture()
	for i := 0; i < 8; i++ {
		tick(t, s, clk)
	}
	want := []domain.SessionID{"coord", "manager-a", "manager-b", "worker-a", "worker-b"}
	if len(mgr.relaunch) != len(want) {
		t.Fatalf("relaunched %v, want %v", mgr.relaunch, want)
	}
	for i := range want {
		if mgr.relaunch[i] != want[i] {
			t.Fatalf("relaunched %v, want %v", mgr.relaunch, want)
		}
	}
}

func TestAtMostOneSessionIsRelaunchedPerTick(t *testing.T) {
	s, mgr, _, _ := fixture()
	s.Tick(context.Background())
	if len(mgr.relaunch) != 1 || mgr.relaunch[0] != "coord" {
		t.Fatalf("one tick relaunched %v, want exactly the coordinator", mgr.relaunch)
	}
}

func TestNeverRelaunchesWhatDoesNotNeedIt(t *testing.T) {
	s, mgr, store, clk := fixture()
	store.proofs["coord"] = true    // already proven
	store.never["manager-a"] = true // auto-resume: never (the Optimus sessions)
	dead := rec("dead", domain.KindWorker, "repo", 9)
	dead.IsTerminated = true
	store.sessions = append(store.sessions, dead) // terminated
	chat := rec("chat-1", domain.KindWorker, "repo", 10)
	chat.HibernatedAt = &epoch // asleep: its wake issues a proof
	store.sessions = append(store.sessions, chat)
	for i := 0; i < 10; i++ {
		tick(t, s, clk)
	}
	for _, id := range mgr.relaunch {
		if id == "coord" || id == "manager-a" || id == "dead" || id == "chat-1" {
			t.Fatalf("relaunched %s, which must be left alone (proven, never, terminated, asleep)", id)
		}
	}
}

// Never mid-turn: a session that is not idle is not touched, and a manager that reports it became
// busy is skipped without being counted as a failure; the next idle candidate goes instead.
func TestNeverRelaunchesARunningTurn(t *testing.T) {
	s, mgr, store, clk := fixture()
	for i := range store.sessions {
		if store.sessions[i].ID == "coord" {
			store.sessions[i].Activity.State = domain.ActivityActive // a turn is running
		}
	}
	tick(t, s, clk)
	if len(mgr.relaunch) != 1 || mgr.relaunch[0] != "manager-a" {
		t.Fatalf("relaunched %v; with the coordinator mid-turn the next idle candidate (a manager) goes", mgr.relaunch)
	}
	// The manager itself says it became busy between the service's look and its own: skipped, no failure.
	mgr.err["manager-b"] = ErrBusy
	tick(t, s, clk)
	if got := mgr.relaunch[len(mgr.relaunch)-1]; got == "manager-b" {
		t.Fatal("a session the manager refused as busy was relaunched")
	}
	if s.failures["manager-b"] != 0 {
		t.Fatalf("a busy refusal counted as %d failures", s.failures["manager-b"])
	}
}

func TestAPrioritizedSessionGoesFirstButStillOnlyIfItNeedsIt(t *testing.T) {
	s, mgr, store, _ := fixture()
	if !s.Prioritize(context.Background(), "worker-b") {
		t.Fatal("a live unproven session was not accepted for priority")
	}
	s.Tick(context.Background())
	if len(mgr.relaunch) != 1 || mgr.relaunch[0] != "worker-b" {
		t.Fatalf("relaunched %v, want the prioritized worker before the coordinator", mgr.relaunch)
	}
	// Priority never overrides the rules.
	store.never["worker-a"] = true
	if s.Prioritize(context.Background(), "worker-a") {
		t.Error("an auto-resume-never session was accepted for priority")
	}
	if s.Prioritize(context.Background(), "worker-b") {
		t.Error("a session that now holds a proof was accepted for priority")
	}
	if s.Prioritize(context.Background(), "nobody") {
		t.Error("an unknown session was accepted for priority")
	}
	if s.Prioritize(context.Background(), "") {
		t.Error("an empty id was accepted for priority")
	}
}

// A failing relaunch is retried with backoff, at most MaxAttempts times, and is then left alone.
func TestAFailingRelaunchBacksOffAndGivesUp(t *testing.T) {
	s, mgr, _, clk := fixture()
	mgr.err["coord"] = errors.New("resume failed")
	s.Tick(context.Background()) // attempt 1
	s.Tick(context.Background()) // too soon: coordinator is backing off, the next candidate goes
	if len(mgr.relaunch) != 1 || mgr.relaunch[0] != "manager-a" {
		t.Fatalf("relaunched %v, want the manager while the coordinator backs off", mgr.relaunch)
	}
	for i := 0; i < 20; i++ {
		clk.now = clk.now.Add(time.Hour)
		s.Tick(context.Background())
	}
	if n := s.failures["coord"]; n != s.cfg.MaxAttempts {
		t.Fatalf("coordinator attempts = %d, want %d then no more", n, s.cfg.MaxAttempts)
	}
	if !s.GaveUp("coord") {
		t.Fatal("the service did not record giving up on the coordinator")
	}
}

// Two sessions of one project are never relaunched in the same window.
func TestAProjectIsNotRelaunchedTwiceInARow(t *testing.T) {
	s, mgr, _, clk := fixture()
	s.cfg.ProjectCooldown = 5 * time.Minute
	for i := 0; i < 3; i++ {
		s.Tick(context.Background())
		clk.now = clk.now.Add(s.cfg.Every + time.Second)
	}
	seen := map[domain.ProjectID]time.Time{}
	_ = seen
	// coord (overall), manager-a (repo), manager-b (repo2): three different projects, all in 3 ticks.
	if len(mgr.relaunch) != 3 {
		t.Fatalf("relaunched %v", mgr.relaunch)
	}
	s.Tick(context.Background()) // worker-a is in repo, relaunched 2 ticks ago: still cooling down
	if got := mgr.relaunch[len(mgr.relaunch)-1]; got == "worker-a" {
		t.Fatalf("worker-a relaunched inside its project's cooldown: %v", mgr.relaunch)
	}
}

func TestNothingHappensBeforeTheStartupDelay(t *testing.T) {
	s, mgr, _, clk := fixture()
	s.started = clk.now // the daemon just started
	s.cfg.StartupDelay = 3 * time.Minute
	s.Tick(context.Background())
	if len(mgr.relaunch) != 0 {
		t.Fatalf("relaunched %v during the startup settle", mgr.relaunch)
	}
	clk.now = clk.now.Add(4 * time.Minute)
	s.Tick(context.Background())
	if len(mgr.relaunch) != 1 {
		t.Fatalf("relaunched %v after the settle", mgr.relaunch)
	}
}

func stoppedOf(s *Service) *fakeStopped { return s.stopped.(*fakeStopped) }

// A relaunch that STOPPED a session and could not start it again leaves it down: the service records
// the intent to bring it back, retries it with backoff, and when it gives up posts the standard
// notice. It is never silent, and the failure is not mistaken for a skip.
func TestASessionLeftDownByAFailedRelaunchIsRetriedRecordedAndFinallyAnnounced(t *testing.T) {
	s, mgr, _, clk := fixture()
	mgr.err["coord"] = fmt.Errorf("%w: provider cannot start", ErrResumeFailed)
	mgr.resumeErr["coord"] = errors.New("still cannot start")
	s.Tick(context.Background()) // the relaunch: stopped, then the start fails
	if got := stoppedOf(s).recorded; len(got) != 1 || got[0] != "coord" {
		t.Fatalf("recorded resume intents = %v, want one for coord", got)
	}
	if len(stoppedOf(s).notified) != 0 {
		t.Fatal("announced before any retry")
	}
	if s.failures["coord"] != 0 || s.GaveUp("coord") {
		t.Fatal("a resume failure was counted as a relaunch failure")
	}
	// retries, with backoff
	s.Tick(context.Background()) // too soon
	if len(mgr.relaunch) != 0 && mgr.relaunch[0] == "coord" {
		t.Fatal("relaunched again")
	}
	for i := 0; i < 20; i++ {
		clk.now = clk.now.Add(time.Hour)
		s.Tick(context.Background())
	}
	if got := stoppedOf(s).notified; len(got) != 1 || got[0] != "coord" {
		t.Fatalf("notices = %v, want exactly one for coord on giving up", got)
	}
	if !s.GaveUp("coord") {
		t.Fatal("not recorded as given up")
	}
	if got := stoppedOf(s).reasons[0]; got == "" || len(got) > 120 {
		t.Fatalf("notice reason %q: want a short fixed phrase", got)
	}
}

func TestASessionLeftDownComesBackOnARetryAndNeverIsAnnounced(t *testing.T) {
	s, mgr, _, clk := fixture()
	mgr.err["coord"] = fmt.Errorf("%w: boom", ErrResumeFailed)
	s.Tick(context.Background())
	delete(mgr.err, "coord") // the provider can start now
	clk.now = clk.now.Add(time.Hour)
	s.Tick(context.Background())
	if len(mgr.resumed) != 1 || mgr.resumed[0] != "coord" {
		t.Fatalf("resumed %v, want coord", mgr.resumed)
	}
	if len(stoppedOf(s).notified) != 0 {
		t.Fatal("announced a session that came back")
	}
	if len(s.down) != 0 {
		t.Fatalf("still tracking %v", s.down)
	}
}

// A session with background work running is never relaunched (and so never killed); the service
// waits, says so once after the max wait, and relaunches it as soon as it is quiet.
func TestASessionWithBackgroundWorkIsWaitedForNeverKilledAndLoggedOnce(t *testing.T) {
	var logs bytes.Buffer
	s, mgr, _, clk := fixture()
	s.log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	for _, id := range []domain.SessionID{"coord", "manager-a", "manager-b", "worker-a", "worker-b"} {
		mgr.err[id] = ErrWorkRunning
	}
	for i := 0; i < 20; i++ { // 200 minutes, past the 2h max wait
		clk.now = clk.now.Add(10 * time.Minute)
		s.Tick(context.Background())
	}
	if len(mgr.relaunch) != 0 {
		t.Fatalf("relaunched %v while background work ran", mgr.relaunch)
	}
	if s.failures["coord"] != 0 || s.GaveUp("coord") {
		t.Fatal("waiting for work was counted as a failure")
	}
	if n := strings.Count(logs.String(), "still has background work running"); n != 5 {
		t.Fatalf("max-wait log entries = %d, want exactly one per waiting session (5), however long it keeps waiting; log:\n%s", n, logs.String())
	}
	// the moment it is quiet it is relaunched
	delete(mgr.err, "coord")
	clk.now = clk.now.Add(time.Minute)
	s.Tick(context.Background())
	if len(mgr.relaunch) != 1 || mgr.relaunch[0] != "coord" {
		t.Fatalf("relaunched %v once the work ended, want coord", mgr.relaunch)
	}
}

// A relaunch that turns out to have nothing to do (the session ended or went to sleep meanwhile) is
// a skip: it is not logged or counted as a relaunch and does not use the project's cooldown.
func TestARelaunchThatDidNothingIsNotReportedAsARelaunch(t *testing.T) {
	var logs bytes.Buffer
	s, mgr, _, clk := fixture()
	s.log = slog.New(slog.NewTextHandler(&logs, nil))
	mgr.noop["coord"] = true
	tick(t, s, clk)
	if strings.Contains(logs.String(), "session=coord ") {
		t.Fatalf("a no-op was logged as a relaunch:\n%s", logs.String())
	}
	if _, cooling := s.lastProject["overall"]; cooling {
		t.Fatal("a no-op started the project cooldown")
	}
}

// Prioritize only queues; it never relaunches, and a busy session that was prioritized is still
// left alone until it is idle.
func TestPrioritizeNeverRelaunchesAndNeverInterruptsAWorkingSession(t *testing.T) {
	s, mgr, store, _ := fixture()
	for i := range store.sessions {
		if store.sessions[i].ID == "worker-b" {
			store.sessions[i].Activity.State = domain.ActivityActive
		}
	}
	s.Prioritize(context.Background(), "worker-b")
	if len(mgr.relaunch) != 0 || len(mgr.asked) != 0 {
		t.Fatalf("Prioritize relaunched or asked about %v", mgr.asked)
	}
	s.Tick(context.Background())
	if mgr.asked["worker-b"] != 0 {
		t.Fatal("a working session was asked to relaunch because it was prioritized")
	}
}
