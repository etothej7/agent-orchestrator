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
	for _, line := range strings.Split(overall, "\n") {
		if strings.Contains(line, "ao spawn") && strings.Contains(line, "--kind orchestrator") && strings.Contains(line, "--client-request-id") {
			t.Fatal("manager prompt advertises a worker-only creation key")
		}
	}
	for _, want := range []string{"Reaching Ed (the human)", "ao ask --id", "ao tell --id", "--kind result|stopped", "Never use it for progress", "At most three --kind result notices", "shown to Ed only when it answers a message Ed typed"} {
		if !strings.Contains(overall, want) {
			t.Fatalf("overall coordinator lacks the human-contact rule %q", want)
		}
	}
	if strings.Contains(overall, "Ordinary assistant output is internal; it does not reach the human") {
		t.Fatal("prompt still says all output is hidden from the human")
	}
	manager := buildSystemPromptText(systemPromptConfig{Role: sessionPromptRoleOrchestrator, Project: promptProject{ID: "api"}, Coordination: assignment})
	if !strings.Contains(manager, "Repo Manager Reporting Chain") || !strings.Contains(manager, `ao report "<summary or suggestion>"`) {
		t.Fatal("repo manager cannot report upward")
	}
	for _, want := range []string{"Ending your turn:", "Do not poll, sleep, loop or check on them", "AO queues a new turn for you when a participant files a phase-ending report", "Do not end your turn with a handoff undelivered"} {
		if !strings.Contains(manager, want) {
			t.Fatalf("repo manager lacks the built-in end-turn rule %q", want)
		}
	}
	if strings.Contains(manager, "end your turn immediately") || !strings.Contains(manager, "Keep working in the same turn for as long as there is authorized work") {
		t.Fatal("end-turn rule must not cut off in-turn assignment, handoff or finding work")
	}
	if !strings.Contains(overall, "end your turn instead of polling managers") || !strings.Contains(overall, "when a repo manager reports to you, and when a story has no assigned manager") {
		t.Fatal("overall coordinator lacks the delegate-then-end-turn rule")
	}
	if strings.Contains(overall, "AO wakes you with their reports and story events") {
		t.Fatal("coordinator prompt promises wakes that story continuations do not send it")
	}
	if strings.Contains(manager, "ao report --message") {
		t.Fatal("prompt uses a nonexistent report CLI flag")
	}
	if !strings.Contains(manager, "parent folders") || !strings.Contains(manager, "Include that workspace guidance") {
		t.Fatal("repo manager must carry parent workspace instructions to external worktrees")
	}
	worker := buildSystemPromptText(systemPromptConfig{Role: sessionPromptRoleWorker, Project: promptProject{ID: "api"}, OrchestratorSessionID: "api-manager", Coordination: assignment})
	if !strings.Contains(worker, "ao send --session api-manager") || strings.Contains(worker, "Overall Coordinator") {
		t.Fatal("worker reporting chain changed")
	}
}
