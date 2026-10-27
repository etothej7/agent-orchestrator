package procmem

import "testing"

func TestShellWorkloadsCountsWhatTheAgentRunsInShellsAndNotItsOwnHelpers(t *testing.T) {
	table, err := Parse(`
  10     1    100 0:01.00 ao chat-host chat-1 /data /work -- codex
  20    10    100 0:01.00 codex app-server
  21    20    100 0:01.00 node mcp-server.js
  30    20    100 0:01.00 /bin/zsh -c go test ./...
  31    30    100 0:01.00 go test ./...
  32    31    100 0:01.00 pkg.test
  40    20    100 0:01.00 -bash
  50     1    100 0:01.00 npm run dev
  60    40    100 0:01.00 sh -c vite
  61    60    100 0:01.00 node vite
`)
	if err != nil {
		t.Fatal(err)
	}
	// outermost shells: 30 (subtree 30,31,32 = 3), 40 (subtree 40,60,61 = 3); 60 is counted with 40.
	if got := table.ShellWorkloads(10); got != 6 {
		t.Fatalf("ShellWorkloads(host) = %d, want 6 (the agent's helpers and an unrelated process do not count)", got)
	}
	if got := table.ShellWorkloads(21); got != 0 {
		t.Fatalf("ShellWorkloads(mcp) = %d, want 0", got)
	}
	if got := table.ShellWorkloads(999); got != 0 {
		t.Fatalf("ShellWorkloads(gone) = %d, want 0", got)
	}
}
