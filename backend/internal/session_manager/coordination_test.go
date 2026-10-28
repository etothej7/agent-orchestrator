package sessionmanager

import (
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestHierarchyPromptsDelegateAtTheCorrectLevel(t *testing.T) {
	assignment := domain.CoordinationConfig{CoordinatorProjectID: "control", ManagedProjectIDs: []domain.ProjectID{"api", "web"}}
	overall := buildSystemPromptText(systemPromptConfig{Role: sessionPromptRoleOrchestrator, Project: promptProject{ID: "control"}, Coordination: assignment})
	for _, required := range []string{"## Overall Coordinator", "- api", "- web", "--kind orchestrator", "not directly to coding workers", "independent review of the exact revision"} {
		if !strings.Contains(overall, required) {
			t.Fatalf("overall coordinator lacks %q", required)
		}
	}
	if strings.Contains(overall, "spawn a freeform worker") {
		t.Fatal("overall coordinator received repo manager delegation instructions")
	}
	if !strings.Contains(overall, `ao spawn --project <assigned-project-id> --kind orchestrator --mode chat --name "Repo manager"`) {
		t.Fatal("manager launch must include the CLI-required name and supported flags")
	}
	manager := buildSystemPromptText(systemPromptConfig{Role: sessionPromptRoleOrchestrator, Project: promptProject{ID: "api"}, Coordination: assignment})
	if !strings.Contains(manager, "Repo Manager Reporting Chain") || !strings.Contains(manager, `ao report "<summary or suggestion>"`) {
		t.Fatal("repo manager cannot report upward")
	}
	if strings.Contains(manager, "ao report --message") {
		t.Fatal("prompt uses a nonexistent report CLI flag")
	}
	if !strings.Contains(manager, "parent folders") || !strings.Contains(manager, "Include that workspace guidance") {
		t.Fatal("repo manager must carry parent workspace instructions to external worktrees")
	}
	// A session re-reads its whole history every turn: one task per conversation.
	for _, want := range []string{"Worker conversations:", "its own new worker session and its own new reviewer session", "never hand them the next task"} {
		if !strings.Contains(manager, want) {
			t.Fatalf("repo manager lacks the one-task-per-conversation rule %q", want)
		}
	}
	if strings.Contains(overall, "Worker conversations:") {
		t.Fatal("the coordinator delegates to managers, not workers, and must not get the worker rule")
	}
	worker := buildSystemPromptText(systemPromptConfig{Role: sessionPromptRoleWorker, Project: promptProject{ID: "api"}, OrchestratorSessionID: "api-manager", Coordination: assignment})
	if !strings.Contains(worker, "ao send --session api-manager") || strings.Contains(worker, "Overall Coordinator") {
		t.Fatal("worker reporting chain changed")
	}
}
