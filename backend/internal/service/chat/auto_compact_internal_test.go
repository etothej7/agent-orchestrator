package chat

import "testing"

// A long-lived Claude Code session re-reads its whole history on every turn, so
// Auto compacts at 200k instead of letting the 1M window fill up.
func TestClaudeAutoCompactTokens(t *testing.T) {
	for _, tt := range []struct {
		name   string
		window string
		env    map[string]string
		model  string
		want   int
	}{
		{"Auto compacts at the default", "", nil, "", 200_000},
		{"Auto on a 1M-window model", "", nil, "claude-sonnet-5-5", 200_000},
		{"Auto on an alias", "", map[string]string{}, "sonnet", 200_000},
		{"explicit 200k", "200k", nil, "", 200_000},
		{"explicit 1m keeps the large window", "1m", nil, "claude-opus-5-5", 1_000_000},
		{"an explicit choice beats the project's environment", "1m", map[string]string{claudeAutoCompactWindowEnv: "150000"}, "", 1_000_000},
		{"Auto leaves a window the project's environment sets", "", map[string]string{claudeAutoCompactWindowEnv: "150000"}, "", 0},
		{"models that stop at 200k need nothing", "", nil, "claude-haiku-5-5", 0},
		{"older 200k-window opus needs nothing", "", nil, "claude-opus-4-5-20251101", 0},
		{"an unknown saved value falls back to Auto", "bogus", nil, "", 200_000},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := claudeAutoCompactTokens(tt.window, tt.env, tt.model); got != tt.want {
				t.Fatalf("claudeAutoCompactTokens(%q, %v, %q) = %d, want %d", tt.window, tt.env, tt.model, got, tt.want)
			}
		})
	}
}
