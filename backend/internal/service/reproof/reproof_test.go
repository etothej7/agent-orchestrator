package reproof

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

type fakeManager struct {
	mu        sync.Mutex
	relaunch  []domain.SessionID
	err       map[domain.SessionID]error
	proofsFor func(domain.SessionID) // called on a successful relaunch (the new launch issues a proof)
}

func (f *fakeManager) RelaunchForIdentity(_ context.Context, id domain.SessionID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.err[id]; err != nil {
		return err
	}
	f.relaunch = append(f.relaunch, id)
	if f.proofsFor != nil {
		f.proofsFor(id)
	}
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
	mgr := &fakeManager{err: map[domain.SessionID]error{}}
	mgr.proofsFor = func(id domain.SessionID) { store.proofs[id] = true }
	clk := &clock{now: epoch.Add(time.Hour)}
	svc := New(mgr, store, slog.New(slog.NewTextHandler(io.Discard, nil)), Config{Now: clk.Now, StartupDelay: -1, ProjectCooldown: time.Millisecond})
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
