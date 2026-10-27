package procidentity

import (
	"context"
	"log/slog"
	"path/filepath"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// AdoptDeps are what the one-time adoption snapshot reads. Everything here is read ONCE, at daemon
// start, for sessions that were already running when identity proofs arrived; nothing is refreshed
// afterwards, so a process planted later (a rewritten host descriptor, a same-name tmux session)
// never becomes a session's root.
type AdoptDeps struct {
	// Sessions are the live sessions to adopt (terminated ones are skipped here).
	Sessions []domain.SessionRecord
	// DataDir is the daemon data dir the chat hosts were started with (a host's argv names it).
	DataDir string
	// TmuxServerPID is AO's own tmux server; a terminal session's pane leader must be its child.
	// Zero means terminals are not adopted.
	TmuxServerPID int
	// PaneRoots returns the pane leader pids of a terminal session's runtime handle.
	PaneRoots func(ctx context.Context, handle string) ([]int, error)
	// ChatDescriptor returns the pid and loopback listen port the chat host recorded for a
	// session. It is a claim by a file any same-user process can write, validated below.
	ChatDescriptor func(id domain.SessionID) (pid, port int, ok bool)
	Procs          ProcTable
	Inspector      ProcInspector
	Listeners      ListenerLookup
	// UID is the daemon's uid; every adopted process must belong to it.
	UID uint32
	Log *slog.Logger
}

// Adopt validates, once, which running processes are the ones the daemon started for each live
// session and returns them as a frozen root list. A candidate that fails ANY check is dropped (that
// session simply stays anonymous):
//
//	terminal: the pane leader's parent is AO's tmux server, it belongs to the daemon's uid, and its
//	          process tree contains the exact supervisor `agent-process supervise --session <id>
//	          --launch <RuntimeLaunchID>` the daemon recorded for that generation;
//	chat:     the descriptor pid is a process whose parent is init/launchd, belongs to the daemon's
//	          uid, whose argv is the AO host subcommand `chat-host <session> <dataDir> ...`, and which
//	          itself owns the loopback socket the descriptor says it listens on.
func Adopt(ctx context.Context, d AdoptDeps) FrozenRoots {
	if d.Procs == nil || d.Inspector == nil {
		return nil
	}
	var all []Proc
	children := map[int][]int{}
	if d.TmuxServerPID > 0 {
		var err error
		if all, err = d.Inspector.All(); err != nil {
			all = nil
		}
		for _, p := range all {
			children[p.PPID] = append(children[p.PPID], p.PID)
		}
	}
	var roots []Root
	for _, rec := range d.Sessions {
		if rec.IsTerminated {
			continue
		}
		var adopted []Root
		if rec.Mode == domain.SessionModeChat {
			adopted = adoptChat(d, rec)
		} else {
			adopted = adoptTerminal(ctx, d, rec, children)
		}
		if len(adopted) == 0 {
			if d.Log != nil {
				d.Log.Debug("session not adopted into the identity bridge", "session", rec.ID)
			}
			continue
		}
		roots = append(roots, adopted...)
	}
	return FrozenRoots(roots)
}

func adoptTerminal(ctx context.Context, d AdoptDeps, rec domain.SessionRecord, children map[int][]int) []Root {
	handle, launch := rec.Metadata.RuntimeHandleID, rec.Metadata.RuntimeLaunchID
	if d.TmuxServerPID <= 0 || d.PaneRoots == nil || handle == "" || launch == "" {
		return nil
	}
	pids, err := d.PaneRoots(ctx, handle)
	if err != nil {
		return nil
	}
	var out []Root
	for _, pid := range pids {
		info, err := d.Procs.Info(pid)
		if err != nil || info.PPID != d.TmuxServerPID || info.UID != d.UID {
			continue
		}
		if !treeRunsSupervisor(d.Inspector, children, pid, string(rec.ID), launch) {
			continue
		}
		out = append(out, Root{Session: rec.ID, PID: pid, Start: info.Start})
	}
	return out
}

// treeRunsSupervisor reports whether root or one of its descendants is the supervisor process
// for exactly this session and launch generation.
func treeRunsSupervisor(insp ProcInspector, children map[int][]int, root int, session, launch string) bool {
	stack := []int{root}
	seen := map[int]bool{}
	for len(stack) > 0 && len(seen) < 512 {
		pid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		if args, err := insp.Args(pid); err == nil && isSupervise(args, session, launch) {
			return true
		}
		stack = append(stack, children[pid]...)
	}
	return false
}

// isSupervise matches `agent-process supervise ... --session <id> --launch <launch> ... --`.
func isSupervise(args []string, session, launch string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] != "agent-process" || args[i+1] != "supervise" {
			continue
		}
		gotSession, gotLaunch := "", ""
		for j := i + 2; j+1 < len(args) && args[j] != "--"; j++ {
			switch args[j] {
			case "--session":
				gotSession = args[j+1]
				j++
			case "--launch":
				gotLaunch = args[j+1]
				j++
			}
		}
		if gotSession == session && gotLaunch == launch {
			return true
		}
	}
	return false
}

func adoptChat(d AdoptDeps, rec domain.SessionRecord) []Root {
	if d.ChatDescriptor == nil || d.Listeners == nil {
		return nil
	}
	pid, port, ok := d.ChatDescriptor(rec.ID)
	if !ok || pid <= 1 || port <= 0 {
		return nil
	}
	info, err := d.Procs.Info(pid)
	if err != nil || info.PPID != 1 || info.UID != d.UID {
		return nil
	}
	args, err := d.Inspector.Args(pid)
	if err != nil || len(args) < 4 || args[1] != "chat-host" || args[2] != string(rec.ID) ||
		filepath.Clean(args[3]) != filepath.Clean(d.DataDir) {
		return nil
	}
	if owner, err := d.Listeners.OwnerOfListener(port); err != nil || owner != pid {
		return nil
	}
	return []Root{{Session: rec.ID, PID: pid, Start: info.Start}}
}
