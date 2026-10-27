package claudeacp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func availableModelsOf(t *testing.T, env map[string]string) []string {
	t.Helper()
	raw, ok := env["CLAUDE_MODEL_CONFIG"]
	if !ok {
		return nil
	}
	var config struct {
		AvailableModels []string `json:"availableModels"`
	}
	if err := json.Unmarshal([]byte(raw), &config); err != nil {
		t.Fatalf("CLAUDE_MODEL_CONFIG = %q: %v", raw, err)
	}
	return config.AvailableModels
}

// The CLI's own default (a short alias such as "sonnet") was missing from the
// allow-list AO builds, so the CLI announced it as restricted on every start.
func TestClaudeACPLaunchEnvIncludesTheUsersDefaultModel(t *testing.T) {
	catalog := []ports.AgentModelInfo{{ID: "claude-opus-4-7"}, {ID: "claude-sonnet-4-6"}}
	cases := []struct {
		name     string
		settings string
		envModel string
		input    map[string]string
		selected string
		models   []ports.AgentModelInfo
		want     []string // nil means no CLAUDE_MODEL_CONFIG change
	}{
		{"default appended after the selected model", `{"model":"sonnet"}`, "", nil, "claude-opus-4-7", catalog,
			[]string{"claude-opus-4-7", "claude-sonnet-4-6", "sonnet"}},
		{"default added when nothing is selected", `{"model":"sonnet"}`, "", nil, "", catalog,
			[]string{"claude-opus-4-7", "claude-sonnet-4-6", "sonnet"}},
		{"selected model is not duplicated", `{"model":"claude-opus-4-7"}`, "", nil, "claude-opus-4-7", catalog,
			[]string{"claude-opus-4-7", "claude-sonnet-4-6"}},
		{"default already in the catalog", `{"model":"claude-sonnet-4-6"}`, "", nil, "", catalog,
			[]string{"claude-opus-4-7", "claude-sonnet-4-6"}},
		{"ANTHROPIC_MODEL in the project env wins over settings", `{"model":"sonnet"}`, "", map[string]string{"ANTHROPIC_MODEL": "opus"}, "claude-opus-4-7", catalog,
			[]string{"claude-opus-4-7", "claude-sonnet-4-6", "opus"}},
		{"ANTHROPIC_MODEL in the daemon env", ``, "haiku", nil, "claude-opus-4-7", catalog,
			[]string{"claude-opus-4-7", "claude-sonnet-4-6", "haiku"}},
		{"blank model", `{"model":"  "}`, "", nil, "claude-opus-4-7", catalog,
			[]string{"claude-opus-4-7", "claude-sonnet-4-6"}},
		{"no model key", `{"theme":"dark"}`, "", nil, "claude-opus-4-7", catalog,
			[]string{"claude-opus-4-7", "claude-sonnet-4-6"}},
		{"malformed settings", `{not json`, "", nil, "claude-opus-4-7", catalog,
			[]string{"claude-opus-4-7", "claude-sonnet-4-6"}},
		{"model is not a string", `{"model":{"a":1}}`, "", nil, "claude-opus-4-7", catalog,
			[]string{"claude-opus-4-7", "claude-sonnet-4-6"}},
		// Native alias selected with no catalog keeps the CLI's full picker, which
		// already contains its default: adding a one-entry list would shrink it.
		{"nothing selected, no catalog", `{"model":"sonnet"}`, "", nil, "", nil, nil},
		{"custom selected model, no catalog", `{"model":"sonnet"}`, "", nil, "claude-opus-4-7", nil,
			[]string{"claude-opus-4-7", "sonnet"}},
		{"native alias, no catalog", `{"model":"sonnet"}`, "", nil, "sonnet", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CLAUDE_MODEL_CONFIG", "")
			t.Setenv("ANTHROPIC_MODEL", tc.envModel)
			prev := readClaudeSettings
			t.Cleanup(func() { readClaudeSettings = prev })
			readClaudeSettings = func(string) ([]byte, error) { return []byte(tc.settings), nil }

			env := claudeACPLaunchEnv(tc.input, "/opt/claude", tc.selected, tc.models)
			if got := availableModelsOf(t, env); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("availableModels = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestClaudeACPLaunchEnvUnreadableSettingsLeavesBehaviourUnchanged(t *testing.T) {
	t.Setenv("CLAUDE_MODEL_CONFIG", "")
	t.Setenv("ANTHROPIC_MODEL", "")
	// TestMain's reader reports "not exist"; behaviour must equal the pre-change output.
	env := claudeACPLaunchEnv(nil, "/opt/claude", "claude-opus-4-7", []ports.AgentModelInfo{{ID: "claude-opus-4-7"}})
	if got := availableModelsOf(t, env); !reflect.DeepEqual(got, []string{"claude-opus-4-7"}) {
		t.Fatalf("availableModels = %v", got)
	}
}

// A list the user wrote is authoritative: their default is theirs to include.
func TestClaudeACPLaunchEnvDoesNotTouchAUserSuppliedAvailableModels(t *testing.T) {
	prev := readClaudeSettings
	t.Cleanup(func() { readClaudeSettings = prev })
	readClaudeSettings = func(string) ([]byte, error) { return []byte(`{"model":"sonnet"}`), nil }
	existing := `{"availableModels":["team-opus"]}`
	env := claudeACPLaunchEnv(map[string]string{"CLAUDE_MODEL_CONFIG": existing}, "/opt/claude", "", []ports.AgentModelInfo{{ID: "claude-opus-4-7"}})
	if env["CLAUDE_MODEL_CONFIG"] != existing {
		t.Fatalf("CLAUDE_MODEL_CONFIG = %q, want it untouched", env["CLAUDE_MODEL_CONFIG"])
	}
}

// Reading is the whole interaction with the user's settings: the production
// reader resolves CLAUDE_CONFIG_DIR and must leave the file exactly as it was.
func TestClaudeACPLaunchEnvReadsSettingsWithoutModifyingThem(t *testing.T) {
	t.Setenv("CLAUDE_MODEL_CONFIG", "")
	t.Setenv("ANTHROPIC_MODEL", "")
	prev := readClaudeSettings
	t.Cleanup(func() { readClaudeSettings = prev })
	readClaudeSettings = defaultReadClaudeSettings

	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	content := []byte(`{"model":"sonnet","theme":"dark"}`)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(path)

	env := claudeACPLaunchEnv(map[string]string{"CLAUDE_CONFIG_DIR": dir}, "/opt/claude", "claude-opus-4-7", []ports.AgentModelInfo{{ID: "claude-opus-4-7"}})
	if got := availableModelsOf(t, env); !reflect.DeepEqual(got, []string{"claude-opus-4-7", "sonnet"}) {
		t.Fatalf("availableModels = %v, want the default read from CLAUDE_CONFIG_DIR/settings.json", got)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	gotBytes, _ := os.ReadFile(path)
	if string(gotBytes) != string(content) || !after.ModTime().Equal(before.ModTime()) || after.Mode() != before.Mode() {
		t.Fatalf("settings.json was modified (bytes equal: %v, mtime equal: %v)", string(gotBytes) == string(content), after.ModTime().Equal(before.ModTime()))
	}
}
