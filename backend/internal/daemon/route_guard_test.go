package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
)

type fakeCoordStore struct {
	cfg      domain.CoordinationConfig
	sessions []domain.SessionRecord
	proofs   map[domain.SessionID]int
	err      error
}

func (f fakeCoordStore) GetCoordination(context.Context) (domain.CoordinationConfig, error) {
	return f.cfg, f.err
}
func (f fakeCoordStore) ListSessions(context.Context, domain.ProjectID) ([]domain.SessionRecord, error) {
	return f.sessions, f.err
}
func (f fakeCoordStore) SessionProofGeneration(_ context.Context, id domain.SessionID) (int, error) {
	return f.proofs[id], f.err
}

func TestTheActiveCoordinatorIsTheNewestLiveOrchestratorOfTheCoordinatorProject(t *testing.T) {
	base := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	store := fakeCoordStore{
		cfg: domain.CoordinationConfig{CoordinatorProjectID: "overall"},
		sessions: []domain.SessionRecord{
			{ID: "old", Kind: domain.KindOrchestrator, CreatedAt: base},
			{ID: "new", Kind: domain.KindOrchestrator, CreatedAt: base.Add(time.Hour)},
			{ID: "dead", Kind: domain.KindOrchestrator, CreatedAt: base.Add(2 * time.Hour), IsTerminated: true},
			{ID: "worker", Kind: domain.KindWorker, CreatedAt: base.Add(3 * time.Hour)},
		},
		proofs: map[domain.SessionID]int{"old": 3},
	}
	st, err := coordinatorProofs{store}.CoordinatorState(context.Background())
	if err != nil || st.Session != "new" || st.HoldsProof {
		t.Fatalf("state = %+v, %v; want the newest live orchestrator (new) with no proof", st, err)
	}
	store.proofs["new"] = 1
	st, _ = coordinatorProofs{store}.CoordinatorState(context.Background())
	if st.Session != "new" || !st.HoldsProof {
		t.Fatalf("after its re-proof: %+v", st)
	}
}

func TestNoCoordinatorMeansNobodyHoldsAProofAndErrorsPropagate(t *testing.T) {
	st, err := coordinatorProofs{fakeCoordStore{}}.CoordinatorState(context.Background())
	if err != nil || st.Session != "" || st.HoldsProof {
		t.Fatalf("no coordinator configured = %+v, %v", st, err)
	}
	if _, err := (coordinatorProofs{fakeCoordStore{err: errors.New("db down")}}).CoordinatorState(context.Background()); err == nil {
		t.Fatal("a store error was swallowed")
	}
}

func TestRouteGuardDefaultsEnforceHumanAndObserveIdentity(t *testing.T) {
	t.Setenv("AO_HUMAN_ENFORCEMENT", "")
	t.Setenv("AO_IDENTITY_ENFORCEMENT", "")
	cfg := routeGuardConfig(fakeCoordStore{})
	if !cfg.EnforceHuman || cfg.Identity != httpd.RouteClassObserve {
		t.Fatalf("defaults = human %v identity %v, want human enforced and identity observed", cfg.EnforceHuman, cfg.Identity)
	}
	t.Setenv("AO_IDENTITY_ENFORCEMENT", "enforce")
	t.Setenv("AO_HUMAN_ENFORCEMENT", "off")
	cfg = routeGuardConfig(fakeCoordStore{})
	if cfg.EnforceHuman || cfg.Identity != httpd.RouteClassEnforce {
		t.Fatalf("overrides = human %v identity %v", cfg.EnforceHuman, cfg.Identity)
	}
}
