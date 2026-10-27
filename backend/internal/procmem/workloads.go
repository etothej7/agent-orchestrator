package procmem

import (
	"path/filepath"
	"strings"
)

// shellNames are the shells agents run tool commands in.
var shellNames = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "fish": true, "ksh": true}

func isShell(command string) bool {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return false
	}
	return shellNames[strings.TrimPrefix(filepath.Base(fields[0]), "-")]
}

// ShellWorkloads counts the processes an agent has running below root: every process in the
// subtree of each OUTERMOST shell under root (the shell included), the way a terminal's tool
// calls, test runs and dev servers show up. An agent's own helper processes (its provider, MCP
// servers) are not shells and are not counted. Restarting the agent would kill these, so a
// restart waits while any run.
func (t *Table) ShellWorkloads(root int) int {
	below := t.Tree(root)
	if len(below.Processes) == 0 {
		return 0
	}
	inTree := map[int]bool{}
	for _, p := range below.Processes {
		inTree[p.PID] = true
	}
	count := 0
	for _, p := range below.Processes {
		if p.PID == root || !isShell(p.Command) {
			continue
		}
		if parent, ok := t.byPID[p.PPID]; ok && parent.PID != root && inTree[parent.PID] && isShell(parent.Command) {
			continue // counted with its outermost shell
		}
		count += len(t.Tree(p.PID).Processes)
	}
	return count
}
