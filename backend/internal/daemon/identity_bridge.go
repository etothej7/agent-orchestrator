package daemon

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/persistenthost"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/identity"
	"github.com/aoagents/agent-orchestrator/backend/internal/procidentity"
)

// bridgeSessions is the slice of the store the bridge reads.
type bridgeSessions interface {
	ListAllSessions(ctx context.Context) ([]domain.SessionRecord, error)
}

// newAncestryBridge builds the live-identity bridge (ED8 D8): it lets the daemon recognise a
// session that was already running when identity proofs arrived by the process tree its caller is
// in. It returns nil, and the daemon simply has no bridge, when the platform lookup is unavailable
// (it self-tests at startup, is macOS-only, and needs no cgo) or AO_PROCESS_ANCESTRY=off.
func newAncestryBridge(log *slog.Logger, sessions bridgeSessions, paneRoots func(ctx context.Context, handle string) ([]int, error), tmuxServerPID func(ctx context.Context) (int, bool), dataDir string) identity.Ancestry {
	procs, peer := newProcessLookup(log)
	return ancestryBridge(log, procs, peer, sessions, paneRoots, tmuxServerPID, dataDir)
}

// newProcessLookup returns the kernel-backed process table and socket lookup, or nils (with a log
// line) when the platform has none or its self-test fails. The same lookup feeds the identity
// bridge and the anonymous-call observations.
func newProcessLookup(log *slog.Logger) (procidentity.ProcTable, procidentity.PeerLookup) {
	procs, peer, err := procidentity.NewSystem()
	if err != nil {
		log.Info("process lookup unavailable", "reason", err.Error())
		return nil, nil
	}
	return procs, peer
}

func ancestryBridge(log *slog.Logger, procs procidentity.ProcTable, peer procidentity.PeerLookup, sessions bridgeSessions, paneRoots func(ctx context.Context, handle string) ([]int, error), tmuxServerPID func(ctx context.Context) (int, bool), dataDir string) identity.Ancestry {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("AO_PROCESS_ANCESTRY")), "off") {
		log.Info("process ancestry identity bridge disabled by AO_PROCESS_ANCESTRY=off")
		return nil
	}
	inspector, ok := procs.(procidentity.ProcInspector)
	listeners, ok2 := peer.(procidentity.ListenerLookup)
	if procs == nil || peer == nil || !ok || !ok2 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	live, err := sessions.ListAllSessions(ctx)
	if err != nil {
		log.Warn("identity bridge: could not list sessions; no running session will be adopted", "error", err.Error())
		return nil
	}
	serverPID := 0
	if tmuxServerPID != nil {
		serverPID, _ = tmuxServerPID(ctx)
	}
	uid := uint32(os.Getuid()) //nolint:gosec // a uid is never negative and fits in 32 bits
	// ONE validated snapshot of the sessions already running at daemon start, frozen: a host
	// descriptor rewritten or a tmux session planted later can never become a session's root.
	roots := procidentity.Adopt(ctx, procidentity.AdoptDeps{
		Sessions:      live,
		DataDir:       dataDir,
		TmuxServerPID: serverPID,
		PaneRoots:     paneRoots,
		ChatDescriptor: func(id domain.SessionID) (int, int, bool) {
			return persistenthost.DescriptorClaim(dataDir, string(id))
		},
		Procs: procs, Inspector: inspector, Listeners: listeners, UID: uid, Log: log,
	})
	log.Info("identity bridge adopted running sessions", "sessions", len(roots))
	resolver := procidentity.NewResolver(procs, peer, roots, uid)
	return procidentity.NewCached(resolver, procidentity.CacheConfig{NegativeTTL: time.Second, MaxEntries: 512})
}
