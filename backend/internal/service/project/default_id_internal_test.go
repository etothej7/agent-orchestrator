package project

import (
	"path/filepath"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestDefaultProjectIDFromFolderName(t *testing.T) {
	cases := map[string]string{
		"agent-orchestrator": "agent-orchestrator",
		"ronish.dev":         "ronish.dev",
		"My_Repo":            "my_repo",
		"My App":             "my-app",
		"app+web":            "app-web",
		"app (v2)":           "app-v2",
		"app@2":              "app-2",
		"app#1":              "app-1",
		"a, b & c":           "a-b-c",
		"café":               "cafe",
		"Résumé_Tool":        "resume_tool",
		"über.dev":           "uber.dev",
		"_tools":             "tools",
		".dotfiles":          "dotfiles",
		"a..b":               "a.b",
		"日本語":                "project",
		"+++":                "project",
	}
	for folder, want := range cases {
		got := defaultProjectID(filepath.Join("/repos", folder))
		if string(got) != want {
			t.Errorf("defaultProjectID(%q) = %q, want %q", folder, got, want)
		}
		if err := validateProjectID(got); err != nil {
			t.Errorf("defaultProjectID(%q) = %q fails validation: %v", folder, got, err)
		}
	}
}

// A pending human request names a project as "project:" + its id and a session by its bare id, and the two must
// never be the same string. That holds because a project id (and so every session id "<project>-<n>") can never
// contain a colon; this pins it where the ids are validated.
func TestAProjectIDNeverContainsAColon(t *testing.T) {
	for _, id := range []string{"project:x", "a:b", ":", "x:", "project:repo-1"} {
		if err := validateProjectID(domain.ProjectID(id)); err == nil {
			t.Errorf("project id %q was accepted", id)
		}
	}
}
