package apispec_test

import (
	"net/http"
	"regexp"
	"sort"
	"strings"
	"testing"

	yaml "gopkg.in/yaml.v3"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
)

// ed.8: a route that RETURNS a credential hands that credential to whoever may call it, so its
// class must be at least as strict as the secret (R2). The mobile group returns the LAN pairing
// password from nine POSTs (disable included) and the status GET; devices and push routes return
// device tokens; remote-host/account-token returns an account token by design. This scan walks
// every 2xx response schema of every operation, any method, and fails when a STRING property whose
// name looks like a secret appears on an operation that is not in the reviewed set below. A new
// secret-bearing response must be classified (and added here) in the same change.
var secretName = regexp.MustCompile(`(?i)(password|token|secret|credential|apikey|api_key|passphrase|bearer)`)

// reviewedSecretBearingResponses is the reviewed set (method + path). Numeric counters such as
// inputTokens are not secrets and are skipped by type.
var reviewedSecretBearingResponses = map[string]bool{
	"GET /api/v1/mobile/status":              true,
	"POST /api/v1/mobile/browser-control":    true,
	"POST /api/v1/mobile/disable":            true,
	"POST /api/v1/mobile/enable":             true,
	"POST /api/v1/mobile/enable-lan-only":    true,
	"POST /api/v1/mobile/enable-tunnel-only": true,
	"POST /api/v1/mobile/keep-awake":         true,
	"POST /api/v1/mobile/regenerate":         true,
	"POST /api/v1/mobile/remote-access":      true,
	"POST /api/v1/mobile/secure-pairing":     true,
	"GET /api/v1/mobile/devices":             true,
	"POST /api/v1/push/devices":              true,
	"DELETE /api/v1/push/devices/{token}":    true,
	"POST /api/v1/remote-host/account-token": true,
}

func schemaSecretProps(schemas map[string]any, node any, seen map[string]bool, depth int) []string {
	m, ok := node.(map[string]any)
	if !ok || depth > 7 {
		return nil
	}
	if ref, ok := m["$ref"].(string); ok {
		name := ref[strings.LastIndex(ref, "/")+1:]
		if seen[name] {
			return nil
		}
		next := map[string]bool{name: true}
		for k := range seen {
			next[k] = true
		}
		return schemaSecretProps(schemas, schemas[name], next, depth+1)
	}
	var out []string
	if props, ok := m["properties"].(map[string]any); ok {
		for k, v := range props {
			if vm, ok := v.(map[string]any); ok && vm["type"] == "string" && secretName.MatchString(k) {
				out = append(out, k)
			}
			for _, sub := range schemaSecretProps(schemas, v, seen, depth+1) {
				out = append(out, k+"."+sub)
			}
		}
	}
	for _, key := range []string{"items", "additionalProperties"} {
		out = append(out, schemaSecretProps(schemas, m[key], seen, depth+1)...)
	}
	for _, key := range []string{"allOf", "oneOf", "anyOf"} {
		if list, ok := m[key].([]any); ok {
			for _, x := range list {
				out = append(out, schemaSecretProps(schemas, x, seen, depth+1)...)
			}
		}
	}
	return out
}

func TestSecretBearingResponsesAreKnown(t *testing.T) {
	var doc struct {
		Paths      map[string]map[string]any `yaml:"paths"`
		Components struct {
			Schemas map[string]any `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(apispec.Default().YAML(), &doc); err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for path, ops := range doc.Paths {
		for method, raw := range ops {
			switch strings.ToUpper(method) {
			case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
			default:
				continue
			}
			op, _ := raw.(map[string]any)
			responses, _ := op["responses"].(map[string]any)
			for code, r := range responses {
				if !strings.HasPrefix(code, "2") {
					continue
				}
				rm, _ := r.(map[string]any)
				content, _ := rm["content"].(map[string]any)
				js, _ := content["application/json"].(map[string]any)
				if len(schemaSecretProps(doc.Components.Schemas, js["schema"], map[string]bool{}, 0)) > 0 {
					found[strings.ToUpper(method)+" "+path] = true
				}
			}
		}
	}
	var unknown, stale []string
	for op := range found {
		if !reviewedSecretBearingResponses[op] {
			unknown = append(unknown, op)
		}
	}
	for op := range reviewedSecretBearingResponses {
		if !found[op] {
			stale = append(stale, op)
		}
	}
	sort.Strings(unknown)
	sort.Strings(stale)
	if len(unknown) > 0 {
		t.Fatalf("responses that return a secret-named string and are not in the reviewed set (classify them human-only or redact, then add them): %v", unknown)
	}
	if len(stale) > 0 {
		t.Fatalf("reviewed secret-bearing responses that no longer return one (remove them from the set): %v", stale)
	}
}
