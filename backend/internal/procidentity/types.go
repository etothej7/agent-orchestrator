// Package procidentity is the daemon's process-ancestry lookup (ED8 D8): given the two ports of
// a loopback connection it finds the process on the other end, walks that process's parent chain
// through the kernel's process table, and says which session's registered root process (a tmux
// pane or a chat host the DAEMON started) the chain reaches.
//
// It exists only as a bridge for sessions that were already running when proof enforcement
// arrived: they have no identity proof in their environment, but the daemon knows which
// processes it launched for them. Everything is derived from the socket and the kernel: nothing
// the request says (a header, a body, AO_SESSION_ID) is an input, and every failure means "no
// session" (fail closed).
package procidentity

import (
	"context"
	"errors"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// Proc is what the kernel says about one process.
type Proc struct {
	PID, PPID int
	// Start is the process start time; together with the pid it identifies one process even when
	// pids are recycled.
	Start time.Time
	UID   uint32
	// Comm is the short process name (diagnostics only).
	Comm string
}

// ProcTable reads the kernel's process table.
type ProcTable interface {
	Info(pid int) (Proc, error)
}

// PeerLookup maps a loopback connection to the pid that owns the client end.
type PeerLookup interface {
	// OwnerPID returns the pid of the process whose socket has local port clientPort and remote
	// port serverPort.
	OwnerPID(clientPort, serverPort int) (int, error)
}

// Root is a process the daemon itself started for a session, recorded with its start time.
type Root struct {
	Session domain.SessionID
	PID     int
	Start   time.Time
}

// RootSource lists the roots the daemon currently has, read from its own records.
type RootSource interface {
	Roots(ctx context.Context) ([]Root, error)
}

// Result is a successful identification.
type Result struct {
	Session domain.SessionID
	// PID and Comm are the calling process (diagnostics).
	PID  int
	Comm string
	// RootPID is the registered root the chain reached.
	RootPID int
}

// Refusals. Each means "this caller is nobody".
var (
	// ErrNoProcess: the process is not in the process table.
	ErrNoProcess = errors.New("process not found")
	// ErrLookup: the socket or the process table could not be read.
	ErrLookup = errors.New("process lookup failed")
	// ErrNotInSession: the caller's chain does not reach any registered root.
	ErrNotInSession = errors.New("caller is not inside any session's process tree")
	// ErrAmbiguous: the chain reaches the roots of more than one session.
	ErrAmbiguous = errors.New("caller's process tree matches more than one session")
	// ErrOtherUser: a process in the chain belongs to another user.
	ErrOtherUser = errors.New("process belongs to another user")
	// ErrInconsistent: the chain is not a plausible parent chain (a parent younger than its
	// child), so the process table changed under the walk.
	ErrInconsistent = errors.New("process chain is inconsistent")
	// ErrTooDeep: the chain did not reach launchd within the depth limit.
	ErrTooDeep = errors.New("process chain is too deep")
)

func domainSession(s string) domain.SessionID { return domain.SessionID(s) }
