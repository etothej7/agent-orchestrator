package daemon

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
	"github.com/aoagents/agent-orchestrator/backend/internal/identity"
	"github.com/aoagents/agent-orchestrator/backend/internal/identityobs"
	"github.com/aoagents/agent-orchestrator/backend/internal/procidentity"
)

// coordinatorStore is the slice of the store the coordinator-proof lookup reads.
type coordinatorStore interface {
	GetCoordination(ctx context.Context) (domain.CoordinationConfig, error)
	ListSessions(ctx context.Context, project domain.ProjectID) ([]domain.SessionRecord, error)
	SessionProofGeneration(ctx context.Context, id domain.SessionID) (int, error)
}

// coordinatorProofs answers, per request, who the active overall coordinator is (the newest live
// orchestrator session of the coordinator project) and whether it holds an identity proof.
type coordinatorProofs struct{ store coordinatorStore }

func (c coordinatorProofs) CoordinatorState(ctx context.Context) (identity.CoordinatorState, error) {
	cfg, err := c.store.GetCoordination(ctx)
	if err != nil {
		return identity.CoordinatorState{}, err
	}
	if cfg.CoordinatorProjectID == "" {
		return identity.CoordinatorState{}, nil // no coordinator configured: nobody holds a proof
	}
	sessions, err := c.store.ListSessions(ctx, cfg.CoordinatorProjectID)
	if err != nil {
		return identity.CoordinatorState{}, err
	}
	var active *domain.SessionRecord
	for i := range sessions {
		rec := &sessions[i]
		if rec.Kind != domain.KindOrchestrator || rec.IsTerminated || rec.Activity.State == domain.ActivityExited {
			continue
		}
		if active == nil || rec.CreatedAt.After(active.CreatedAt) {
			active = rec
		}
	}
	if active == nil {
		return identity.CoordinatorState{}, nil
	}
	gen, err := c.store.SessionProofGeneration(ctx, active.ID)
	if err != nil {
		return identity.CoordinatorState{}, err
	}
	return identity.CoordinatorState{Session: active.ID, HoldsProof: gen > 0}, nil
}

// routeGuardConfig is the daemon's route-class guard setting (ED8): class-H routes require the
// human capability (AO_HUMAN_ENFORCEMENT=off is a scratch/diagnostic escape that only the process
// that starts the daemon can set), and identity (class A) is observed unless
// AO_IDENTITY_ENFORCEMENT=enforce, which is its own reviewed build to turn on by default.
func routeGuardConfig(store coordinatorStore) httpd.GuardConfig {
	return httpd.GuardConfig{
		EnforceHuman: !strings.EqualFold(strings.TrimSpace(os.Getenv("AO_HUMAN_ENFORCEMENT")), "off"),
		Identity:     httpd.ParseRouteClassMode(os.Getenv("AO_IDENTITY_ENFORCEMENT")),
		Coordinator:  coordinatorProofs{store: store},
	}
}

// guardObserver feeds the route-class guard's anonymous-call events into the observations and the
// log. It never blocks a request: the recorder does its lookup off the request path.
type guardObserver struct{ rec *identityobs.Recorder }

func (g guardObserver) ObserveAnonymous(c httpd.AnonymousCall) {
	g.rec.Record(identityobs.Call{
		Method: c.Method, Route: c.Route, Class: string(c.Class), Claimed: c.Claimed,
		Allowed: c.Allowed, Reason: c.Reason, ClientPort: c.ClientPort, ServerPort: c.ServerPort,
	})
}

// newIdentityObservations builds the recorder behind the top-offenders view. Each lookup outcome is
// logged: refusals at info (they are what to fix), allowed calls at info the first time a process
// is seen on a route and at debug after that, so a chatty tool does not flood the log.
func newIdentityObservations(log *slog.Logger, procs procidentity.ProcTable, peer procidentity.PeerLookup) *identityobs.Recorder {
	var mu sync.Mutex
	seen := map[string]bool{}
	return identityobs.New(peer, procs, identityobs.Config{
		OnLookup: func(c identityobs.Call, pid int, name string) {
			key := c.Method + " " + c.Route + " " + name
			mu.Lock()
			first := !seen[key] && len(seen) < 4096
			if first {
				seen[key] = true
			}
			mu.Unlock()
			attrs := []any{"method", c.Method, "route", c.Route, "class", c.Class, "allowed", c.Allowed, "reason", c.Reason, "claimed_session", string(c.Claimed), "pid", pid, "process", name}
			if !c.Allowed || first {
				log.Info("call without an identity", attrs...)
				return
			}
			log.Debug("call without an identity", attrs...)
		},
	})
}

func identityModeName(m httpd.RouteClassMode) string {
	if m == httpd.RouteClassEnforce {
		return "enforce"
	}
	return "observe"
}
