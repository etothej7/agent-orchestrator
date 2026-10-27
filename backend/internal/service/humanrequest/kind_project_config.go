package humanrequest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
)

// ---- project_config ----

// ProjectConfigWriter stores a project's whole configuration. The service calls it when the person approves
// a project_config request, under the approving request's context (which holds the human capability), so the
// project service's own protections see the person.
type ProjectConfigWriter interface {
	SetProjectConfig(ctx context.Context, projectID string, cfg domain.ProjectConfig) error
}

// SetProjectConfigWriter gives the service the action that stores a project's configuration, once the daemon
// has built the project service. Without it the project_config kind is refused as unavailable.
func (s *Service) SetProjectConfigWriter(w ProjectConfigWriter) {
	s.pcwMu.Lock()
	defer s.pcwMu.Unlock()
	s.pcw = w
}

func (s *Service) projectConfigWriter() ProjectConfigWriter {
	s.pcwMu.RLock()
	defer s.pcwMu.RUnlock()
	return s.pcw
}

// projectConfig is a request to change protected fields of one project's configuration: commands AO runs
// (postCreate, preRemove), environment, standing rules, permissions, and the like. The requester names the
// fields and the values they want; the daemon reads the current values itself, shows the person exactly what
// would change, and refuses to apply if any of those fields changed after the request was filed.
type projectConfig struct{}

// projectConfigInput is what a requester supplies: nothing but the protected fields and their new values.
type projectConfigInput struct {
	Set map[string]json.RawMessage `json:"set"`
}

type projectConfigChange struct {
	Key  string          `json:"key"`
	From json.RawMessage `json:"from"`
	To   json.RawMessage `json:"to"`
}

// projectConfigPayload is the canonical payload the daemon builds and the person's approval is bound to.
type projectConfigPayload struct {
	Changes []projectConfigChange `json:"changes"`
}

const (
	maxProjectConfigKeys  = 12
	maxProjectConfigBytes = 32 << 10
	maxShownValueRunes    = 600
)

func (projectConfig) prepareProject(_ context.Context, s *Service, target domain.ProjectRecord, raw json.RawMessage) (prepared, error) {
	if s.projectConfigWriter() == nil {
		return prepared{}, apierr.NotImplemented("HUMAN_REQUEST_KIND_UNAVAILABLE", "changing project settings is not available on this daemon")
	}
	if len(raw) > maxProjectConfigBytes {
		return prepared{}, apierr.Invalid("HUMAN_REQUEST_PAYLOAD_INVALID", "the requested settings are too large", nil)
	}
	var in projectConfigInput
	if err := decodeStrict(raw, &in); err != nil {
		return prepared{}, err
	}
	if len(in.Set) == 0 || len(in.Set) > maxProjectConfigKeys {
		return prepared{}, apierr.Invalid("HUMAN_REQUEST_PAYLOAD_INVALID", "a project_config request names between 1 and 12 protected fields in set", nil)
	}
	for key := range in.Set {
		if !domain.IsProtectedConfigKey(key) {
			return prepared{}, apierr.Invalid("HUMAN_REQUEST_PAYLOAD_INVALID", fmt.Sprintf("%q is not a protected project setting; safe settings need no request", key), nil)
		}
	}
	proposed, err := overlayConfig(target.Config, in.Set)
	if err != nil {
		return prepared{}, apierr.Invalid("HUMAN_REQUEST_PAYLOAD_INVALID", "the requested settings are not valid: "+err.Error(), nil)
	}
	before, after := configJSON(target.Config), configJSON(proposed)
	var keys []string
	for key := range in.Set {
		if !reflect.DeepEqual(before[key], after[key]) {
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		return prepared{}, apierr.Conflict("HUMAN_REQUEST_NOT_NEEDED", "those settings already have the requested values", nil)
	}
	sort.Strings(keys)
	payload := projectConfigPayload{}
	changes := make([]domain.HumanRequestChange, 0, len(keys))
	for _, key := range keys {
		from, to := mustJSON(before[key]), mustJSON(after[key])
		payload.Changes = append(payload.Changes, projectConfigChange{Key: key, From: from, To: to})
		changes = append(changes, domain.HumanRequestChange{Field: key, From: shownValue(from), To: shownValue(to)})
	}
	canonicalPayload, err := canonical(payload)
	if err != nil {
		return prepared{}, err
	}
	return prepared{
		payload: canonicalPayload,
		summary: fmt.Sprintf("Change protected settings of project %s: %s. They decide which commands AO runs, which environment sessions get, which rules they follow or what an agent may do.", projectLabel(target), strings.Join(keys, ", ")),
		changes: changes,
	}, nil
}

func (projectConfig) applyProject(ctx context.Context, s *Service, req domain.HumanRequest, target domain.ProjectRecord) (string, error) {
	writer := s.projectConfigWriter()
	if writer == nil {
		return "", errors.New("changing project settings is not available on this daemon")
	}
	var in projectConfigPayload
	if err := json.Unmarshal(req.Payload, &in); err != nil {
		return "", err
	}
	current := configJSON(target.Config)
	set := map[string]json.RawMessage{}
	keys := make([]string, 0, len(in.Changes))
	for _, change := range in.Changes {
		if !domain.IsProtectedConfigKey(change.Key) {
			return "", fmt.Errorf("%q is not a protected project setting", change.Key)
		}
		if !reflect.DeepEqual(current[change.Key], decodeAny(change.From)) {
			return "", staleError{reason: "the project's " + change.Key + " changed after this was filed; ask again to see the current value"}
		}
		set[change.Key] = change.To
		keys = append(keys, change.Key)
	}
	next, err := overlayConfig(target.Config, set)
	if err != nil {
		return "", err
	}
	if err := writer.SetProjectConfig(ctx, target.ID, next); err != nil {
		return "", err
	}
	return "protected settings changed: " + strings.Join(keys, ", "), nil
}

// overlayConfig returns base with the given top-level keys replaced by the given JSON values. Unknown fields
// inside a value are refused, so a typo can never silently drop part of what the person approved.
func overlayConfig(base domain.ProjectConfig, set map[string]json.RawMessage) (domain.ProjectConfig, error) {
	m := configJSON(base)
	for key, raw := range set {
		m[key] = decodeAny(raw)
	}
	merged, err := json.Marshal(m)
	if err != nil {
		return domain.ProjectConfig{}, err
	}
	var out domain.ProjectConfig
	if err := decodeStrictConfig(merged, &out); err != nil {
		return domain.ProjectConfig{}, err
	}
	return out, nil
}

func decodeStrictConfig(raw []byte, out *domain.ProjectConfig) error {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	return dec.Decode(out)
}

func configJSON(c domain.ProjectConfig) map[string]any {
	raw, _ := json.Marshal(c)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	if m == nil {
		m = map[string]any{}
	}
	return m
}

func decodeAny(raw json.RawMessage) any {
	var v any
	_ = json.Unmarshal(raw, &v)
	return v
}

func mustJSON(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil || string(raw) == "null" {
		return json.RawMessage("null")
	}
	return raw
}

// shownValue renders a value for the card: the data itself (the person must see the exact commands, variables
// or rules), cleaned of control and direction characters, bounded, never as text the requester composed.
func shownValue(raw json.RawMessage) string {
	if string(raw) == "null" {
		return "(not set)"
	}
	return cleanLabel(string(raw), maxShownValueRunes)
}
