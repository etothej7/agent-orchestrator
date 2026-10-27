package procidentity

import (
	"context"
	"fmt"
)

// maxDepth bounds the parent walk. A real chain (launchd, tmux server, pane shell, agent,
// tools, ao) is under ten hops; anything deeper is not trusted.
const maxDepth = 32

// Resolver identifies the session a loopback caller belongs to.
type Resolver struct {
	procs     ProcTable
	peer      PeerLookup
	roots     RootSource
	daemonUID uint32
}

// NewResolver builds a Resolver. daemonUID is the user the daemon runs as: processes of any
// other user are never a session.
func NewResolver(procs ProcTable, peer PeerLookup, roots RootSource, daemonUID uint32) *Resolver {
	return &Resolver{procs: procs, peer: peer, roots: roots, daemonUID: daemonUID}
}

// Identify maps a loopback connection (the client's ephemeral port and the daemon's listening
// port) to the session whose process tree the caller is in. It fails closed: any lookup error, a
// pid recycled since registration, a chain that ends at launchd without reaching a registered
// root (a double-forked or reparented process), a process of another user, an inconsistent chain
// and an ambiguous lineage all return no session and an error.
func (r *Resolver) Identify(ctx context.Context, clientPort, serverPort int) (Result, error) {
	pid, err := r.peer.OwnerPID(clientPort, serverPort)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %w", ErrLookup, err)
	}
	roots, err := r.roots.Roots(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %w", ErrLookup, err)
	}
	byPID := map[int][]Root{}
	for _, root := range roots {
		if root.PID > 1 && root.Session != "" {
			byPID[root.PID] = append(byPID[root.PID], root)
		}
	}

	res := Result{PID: pid}
	matched := map[string]int{} // distinct sessions reached -> the root pid that matched
	cur := pid
	var childStart int64
	reachedLaunchd := false
	for hop := 0; hop <= maxDepth; hop++ {
		info, err := r.procs.Info(cur)
		if err != nil {
			return Result{}, fmt.Errorf("%w: %w", ErrLookup, err)
		}
		if hop == 0 {
			res.Comm = info.Comm
		}
		if cur <= 1 {
			reachedLaunchd = true
			break
		}
		if info.UID != r.daemonUID {
			return Result{}, ErrOtherUser
		}
		start := info.Start.UnixMicro()
		if hop > 0 && start > childStart {
			return Result{}, ErrInconsistent
		}
		childStart = start
		for _, root := range byPID[cur] {
			if root.Start.UnixMicro() == start { // same pid AND same start time: the same process
				matched[string(root.Session)] = cur
			}
		}
		if info.PPID == cur {
			return Result{}, ErrInconsistent
		}
		cur = info.PPID
	}
	if !reachedLaunchd {
		return Result{}, ErrTooDeep
	}
	switch len(matched) {
	case 0:
		return Result{}, ErrNotInSession
	case 1:
		for session, rootPID := range matched {
			res.Session = domainSession(session)
			res.RootPID = rootPID
		}
		return res, nil
	default:
		return Result{}, ErrAmbiguous
	}
}
