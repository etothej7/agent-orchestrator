package domain

import (
	"encoding/json"
	"reflect"
	"sort"
)

// Protected project configuration (ED8). A project's config is written by several routes, and some of
// its fields make AO run commands, widen what an agent may do, or change what every worker is told, so
// only the person (the human capability) may change them. The rest are safe defaults (a model, an
// effort, a branch name) that the coordinator and a project's manager may set directly.
//
// The split is an ALLOWLIST of safe fields, not a list of dangerous ones: a field added to
// ProjectConfig later is protected until someone decides it is safe, so the default fails closed.
//
// safeTopLevel are config keys whose value is safe whatever it is.
var safeTopLevel = map[string]bool{
	"defaultBranch":        true,
	"sessionPrefix":        true,
	"autoReview":           true,
	"workersRequestReview": true,
}

// safeAgentConfigKeys are the parts of an agent config that only choose a model and how hard it thinks.
// Permissions (and anything added later) are protected.
var safeAgentConfigKeys = map[string]bool{"model": true, "effort": true, "mode": true}

// nestedAgentConfig lists the config keys that hold an agent config, directly ("agentConfig") or inside
// a role override ("worker", "orchestrator", whose own "agent" harness choice is also safe).
var nestedAgentConfig = map[string]bool{"agentConfig": true, "worker": true, "orchestrator": true}

// ProtectedConfigChanges returns the sorted names of the PROTECTED config keys that differ between
// two configs (empty when only safe fields changed). A nested key is reported by its top-level name
// (for example "agentConfig" when permissions changed).
func ProtectedConfigChanges(before, after ProjectConfig) []string {
	b, a := configMap(before), configMap(after)
	var changed []string
	seen := map[string]bool{}
	for key := range b {
		seen[key] = true
	}
	for key := range a {
		seen[key] = true
	}
	for key := range seen {
		if safeTopLevel[key] {
			continue
		}
		bv, av := b[key], a[key]
		if nestedAgentConfig[key] {
			bv, av = stripSafeAgentConfig(key, bv), stripSafeAgentConfig(key, av)
		}
		if !reflect.DeepEqual(bv, av) {
			changed = append(changed, key)
		}
	}
	sort.Strings(changed)
	return changed
}

// WithSafeChangesOnly returns base with every SAFE field taken from proposed and every protected field
// left as base has it: what a caller who may not change protected fields is allowed to do with a
// proposed config.
func WithSafeChangesOnly(base, proposed ProjectConfig) ProjectConfig {
	out := base
	out.DefaultBranch = proposed.DefaultBranch
	out.SessionPrefix = proposed.SessionPrefix
	out.AutoReview = proposed.AutoReview
	out.WorkersRequestReview = proposed.WorkersRequestReview
	out.AgentConfig = withSafeAgentConfig(base.AgentConfig, proposed.AgentConfig)
	out.Worker.Harness = proposed.Worker.Harness
	out.Worker.AgentConfig = withSafeAgentConfig(base.Worker.AgentConfig, proposed.Worker.AgentConfig)
	out.Orchestrator.Harness = proposed.Orchestrator.Harness
	out.Orchestrator.AgentConfig = withSafeAgentConfig(base.Orchestrator.AgentConfig, proposed.Orchestrator.AgentConfig)
	return out
}

func withSafeAgentConfig(base, proposed AgentConfig) AgentConfig {
	out := base
	out.Model, out.Effort, out.Mode = proposed.Model, proposed.Effort, proposed.Mode
	return out
}

func configMap(c ProjectConfig) map[string]any {
	raw, err := json.Marshal(c)
	if err != nil {
		return map[string]any{"unreadable": true}
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return map[string]any{"unreadable": true}
	}
	return m
}

// stripSafeAgentConfig removes the safe sub-keys of an agent config (or of a role override holding one)
// so that what remains is exactly the protected part.
func stripSafeAgentConfig(key string, v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	out := make(map[string]any, len(m))
	for k, val := range m {
		out[k] = val
	}
	if key == "agentConfig" {
		for k := range safeAgentConfigKeys {
			delete(out, k)
		}
		return nilIfEmpty(out)
	}
	delete(out, "agent") // a role's harness choice is safe
	if inner, ok := out["agentConfig"].(map[string]any); ok {
		out["agentConfig"] = stripSafeAgentConfig("agentConfig", inner)
	}
	return nilIfEmpty(out)
}

func nilIfEmpty(m map[string]any) any {
	for k, v := range m {
		if v == nil {
			delete(m, k)
		}
	}
	if len(m) == 0 {
		return nil
	}
	return m
}
