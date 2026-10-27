package project_test

import (
	"context"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/identity"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/project"
)

func mayMerge(t *testing.T, m project.Manager, id domain.ProjectID) bool {
	t.Helper()
	got, err := m.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return got.Project != nil && got.Project.Config != nil && got.Project.Config.AgentMayMerge
}

func callerCtx(c identity.Caller) context.Context {
	return identity.WithCaller(context.Background(), c)
}

// The setting that lets agents merge must never be something an agent can switch on for itself, through any of
// the routes that write a project's config.
func TestOnlyThePersonCanChangeWhetherAgentsMayMerge(t *testing.T) {
	m := newManager(t)
	person := callerCtx(identity.Human())
	repo := gitRepo(t)
	if _, err := m.Add(person, project.AddInput{Path: repo, ProjectID: ptr("ao")}); err != nil {
		t.Fatal(err)
	}
	if mayMerge(t, m, "ao") {
		t.Fatal("a new project lets agents merge")
	}

	callers := map[string]context.Context{
		"an agent session": callerCtx(identity.Caller{Kind: identity.KindAgent, Session: "ao-1", SessionKind: domain.KindOrchestrator, Project: "ao"}),
		"the coordinator":  callerCtx(identity.Caller{Kind: identity.KindAgent, Session: "coord-1", SessionKind: domain.KindOrchestrator}),
		"a reviewer":       callerCtx(identity.Caller{Kind: identity.KindReviewer, Review: "rev-1", Worker: "ao-1"}),
		"nobody":           callerCtx(identity.Anonymous()),
		"a bare context":   context.Background(),
	}
	on := domain.ProjectConfig{AgentMayMerge: true, AgentRules: "x"}
	for name, ctx := range callers {
		if _, err := m.SetConfig(ctx, "ao", project.SetConfigInput{Config: on}); err == nil {
			t.Errorf("%s: SetConfig(agentMayMerge:true) was accepted", name)
		}
		if _, err := m.UpdateSettings(ctx, "ao", project.UpdateSettingsInput{DisplayName: "ao", Config: on}); err == nil {
			t.Errorf("%s: UpdateSettings(agentMayMerge:true) was accepted", name)
		}
		if _, err := m.Add(ctx, project.AddInput{Path: gitRepo(t), ProjectID: ptr("new-" + string(rune('a'+len(name)%20))), Config: &on}); err == nil {
			t.Errorf("%s: Add(agentMayMerge:true) was accepted", name)
		}
		if mayMerge(t, m, "ao") {
			t.Fatalf("%s turned agentMayMerge on", name)
		}
	}

	// The person can.
	if _, err := m.SetConfig(person, "ao", project.SetConfigInput{Config: on}); err != nil || !mayMerge(t, m, "ao") {
		t.Fatalf("the person could not turn it on: %v", err)
	}

	// With it on, an agent's rewrite of the rest of the config neither revokes nor keeps changing it, whatever it says.
	agent := callers["an agent session"]
	for _, cfg := range []domain.ProjectConfig{{AgentRules: "agent-edited"}, {AgentMayMerge: false, AgentRules: "agent-edited"}, {AgentMayMerge: true, AgentRules: "agent-edited"}} {
		if _, err := m.SetConfig(agent, "ao", project.SetConfigInput{Config: cfg}); err != nil {
			t.Fatalf("an agent's ordinary config write was refused: %v", err)
		}
		if !mayMerge(t, m, "ao") {
			t.Fatalf("an agent's rewrite %+v revoked the person's choice", cfg)
		}
	}
	if got, _ := m.Get(context.Background(), "ao"); got.Project == nil || got.Project.Config == nil || got.Project.Config.AgentRules != "agent-edited" {
		t.Fatalf("the agent's other changes were not applied: %+v", got.Project)
	}

	// And the person can turn it off again.
	if _, err := m.SetConfig(person, "ao", project.SetConfigInput{Config: domain.ProjectConfig{}}); err != nil || mayMerge(t, m, "ao") {
		t.Fatalf("the person could not turn it off: %v", err)
	}
}
