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
func newAncestryBridge(log *slog.Logger, sessions bridgeSessions, paneRoots func(ctx context.Context, handle string) ([]int, error), dataDir string) identity.Ancestry {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("AO_PROCESS_ANCESTRY")), "off") {
		log.Info("process ancestry identity bridge disabled by AO_PROCESS_ANCESTRY=off")
		return nil
	}
	procs, peer, err := procidentity.NewSystem()
	if err != nil {
		log.Info("process ancestry identity bridge unavailable", "reason", err.Error())
		return nil
	}
	roots := procidentity.NewSessionRoots(procidentity.SessionRootsDeps{
		List:      sessions.ListAllSessions,
		PaneRoots: paneRoots,
		ChatHostPID: func(id domain.SessionID) (int, bool) {
			return persistenthost.HostPID(dataDir, string(id))
		},
		Procs: procs,
		TTL:   5 * time.Second,
	})
	uid := uint32(os.Getuid()) //nolint:gosec // a uid is never negative and fits in 32 bits
	resolver := procidentity.NewResolver(procs, peer, roots, uid)
	return procidentity.NewCached(resolver, procs, procidentity.CacheConfig{TTL: 2 * time.Second, NegativeTTL: time.Second, MaxEntries: 512})
}
