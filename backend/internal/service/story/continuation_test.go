package story

import (
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"testing"
)

func TestContinuationReviewRemediationCycleUsesCurrentRevision(t *testing.T) {
	s := domain.Story{Assignments: []domain.StoryAssignment{{SessionID: "manager", Role: "manager"}, {SessionID: "developer", Role: "worker"}, {SessionID: "reviewer", Role: "reviewer"}}}
	var events []domain.StoryEvent
	steps := []struct {
		event      domain.StoryEvent
		checkpoint string
	}{
		{domain.StoryEvent{Kind: "report", Result: "done"}, "implementation"},
		{domain.StoryEvent{Kind: "implementation", SenderSessionID: "developer", Reference: "A"}, "verify:A"},
		{domain.StoryEvent{Kind: "testing", Reference: "A", Result: "passed"}, "independent-review:A"},
		{domain.StoryEvent{ID: "finding-A", Kind: "review_failed", SenderSessionID: "reviewer", Reference: "A"}, "remediate:A:finding-A"},
		{domain.StoryEvent{Kind: "implementation", SenderSessionID: "developer", Reference: "B"}, "verify:B"},
		{domain.StoryEvent{Kind: "review_passed", SenderSessionID: "reviewer", Reference: "A"}, "verify:B"},
		{domain.StoryEvent{Kind: "testing", Reference: "B", Result: "failed"}, "verify:B"},
		{domain.StoryEvent{Kind: "testing", Reference: "B", Result: "passed"}, "independent-review:B"},
		{domain.StoryEvent{Kind: "review_passed", SenderSessionID: "reviewer", Reference: "B"}, "complete-scope:B"},
	}
	for _, step := range steps {
		events = append(events, step.event)
		if got := continuationCheckpoint(s, events); got != step.checkpoint {
			t.Fatalf("after %+v: got %s want %s", step.event, got, step.checkpoint)
		}
		if s.Status == "done" {
			t.Fatal("continuation manufactured completion")
		}
	}
}
