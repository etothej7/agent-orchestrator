package humanrequest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// prepared is what a kind renders from a payload: the canonical payload that gets hashed, and the
// summary and diff the person will read. All three come from the payload and the current state,
// never from text the requester wrote.
type prepared struct {
	payload json.RawMessage
	summary string
	changes []domain.HumanRequestChange
}

type kindHandler interface {
	prepare(ctx context.Context, s *Service, target domain.SessionRecord, raw json.RawMessage) (prepared, error)
	apply(ctx context.Context, s *Service, req domain.HumanRequest, target domain.SessionRecord) (string, error)
}

var kinds = map[domain.HumanRequestKind]kindHandler{
	domain.HumanRequestAutoResumeLift:       autoResumeLift{},
	domain.HumanRequestConversationSettings: conversationSettings{},
	domain.HumanRequestProviderSwitch:       providerSwitch{},
}

// projectKindHandler is a kind that is about a project rather than a session (a protected change to a
// project's configuration, removing a project). It renders and applies exactly as a session kind does.
type projectKindHandler interface {
	prepareProject(ctx context.Context, s *Service, target domain.ProjectRecord, raw json.RawMessage) (prepared, error)
	applyProject(ctx context.Context, s *Service, req domain.HumanRequest, target domain.ProjectRecord) (string, error)
}

// projectKinds are the kinds about a project: merging a pull request on a project that has not let agents merge.
// More (protected project-config fields, removing a project) land here; the plumbing is also exercised by a
// stand-in kind registered in the tests.
var projectKinds = map[domain.HumanRequestKind]projectKindHandler{
	domain.HumanRequestPRMerge:       prMerge{},
	domain.HumanRequestProjectConfig: projectConfig{},
}

// KnownKinds lists the kinds a requester may file.
func KnownKinds() []domain.HumanRequestKind {
	out := make([]domain.HumanRequestKind, 0, len(kinds)+len(projectKinds))
	for kind := range kinds {
		out = append(out, kind)
	}
	for kind := range projectKinds {
		out = append(out, kind)
	}
	slices.Sort(out)
	return out
}

// errStale means the world moved between filing and approving, so the diff the person saw no
// longer describes what approving would do. The request fails and the requester asks again.
type staleError struct{ reason string }

func (e staleError) Error() string { return e.reason }

func isStale(err error) bool {
	var stale staleError
	return errors.As(err, &stale)
}

// maxNameRunes bounds how much of a session's display name the card shows.
const maxNameRunes = 40

// sessionLabel names the target of a request: the id first, which nobody can rename, then the display name
// as a quoted label. A display name is agent-writable (any agent may rename its own session or title a
// delegation), so it is cleaned of control, line-break and direction-override characters, collapsed to one
// line, bounded, and presented as "named ...", never as a sentence the person might take for the daemon's.
func sessionLabel(rec domain.SessionRecord) string {
	name := cleanLabel(rec.DisplayName, maxNameRunes)
	if name == "" {
		return string(rec.ID)
	}
	return fmt.Sprintf("%s (named %q)", rec.ID, name)
}

// projectLabel names a project as sessionLabel names a session: the id first, then the display name as a
// quoted, cleaned, bounded label. A project's display name is as agent-writable as a session's.
func projectLabel(rec domain.ProjectRecord) string {
	name := cleanLabel(rec.DisplayName, maxNameRunes)
	if name == "" || name == rec.ID {
		return rec.ID
	}
	return fmt.Sprintf("%s (named %q)", rec.ID, name)
}

func cleanLabel(raw string, limit int) string {
	var b strings.Builder
	space := false
	count := 0
	for _, r := range raw {
		switch {
		case unicode.IsSpace(r), unicode.Is(unicode.Zl, r), unicode.Is(unicode.Zp, r):
			space = b.Len() > 0
			continue
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r), unicode.Is(unicode.Co, r), unicode.Is(unicode.Cs, r), r == unicode.ReplacementChar:
			continue
		case r == '"':
			r = '\''
		case r == '\\':
			r = '/'
		}
		if space {
			b.WriteRune(' ')
			count++
			space = false
		}
		if count >= limit {
			b.WriteString("…")
			return b.String()
		}
		b.WriteRune(r)
		count++
	}
	return b.String()
}

// tokenPattern is the whole vocabulary of a model or effort the person is shown that no catalog vouches for:
// the shape of a real model id, nothing a requester could use to write a sentence.
var tokenPattern = regexp.MustCompile(`^[A-Za-z0-9._:/-]{1,64}$`)

func validToken(value string) bool { return tokenPattern.MatchString(value) }

// decodeStrict reads a payload into out, refusing unknown fields and trailing data.
func decodeStrict(raw json.RawMessage, out any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage(`{}`)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return apierr.Invalid("HUMAN_REQUEST_PAYLOAD_INVALID", "the request payload is not valid for this kind: "+err.Error(), nil)
	}
	if dec.More() {
		return apierr.Invalid("HUMAN_REQUEST_PAYLOAD_INVALID", "the request payload has data after the object", nil)
	}
	return nil
}

func canonical(v any) (json.RawMessage, error) {
	raw, err := json.Marshal(v)
	return json.RawMessage(raw), err
}

// ---- auto_resume_lift ----

type autoResumeLift struct{}

type emptyPayload struct{}

func (autoResumeLift) prepare(ctx context.Context, s *Service, target domain.SessionRecord, raw json.RawMessage) (prepared, error) {
	if s.autoResume == nil {
		return prepared{}, apierr.NotImplemented("HUMAN_REQUEST_KIND_UNAVAILABLE", "auto-resume is not available on this daemon")
	}
	if err := decodeStrict(raw, &emptyPayload{}); err != nil {
		return prepared{}, err
	}
	policy, err := s.autoResume.AutoResumePolicy(ctx, target.ID)
	if err != nil {
		return prepared{}, err
	}
	if policy.Allows() {
		return prepared{}, apierr.Conflict("HUMAN_REQUEST_NOT_NEEDED", "automatic resume is already allowed for this session", nil)
	}
	payload, err := canonical(emptyPayload{})
	if err != nil {
		return prepared{}, err
	}
	return prepared{
		payload: payload,
		summary: fmt.Sprintf("Allow automatic resume for %s. It is protected today: AO will not resume it, replace it or tear it down on its own. If you allow it, AO may do all three again.", sessionLabel(target)),
		changes: []domain.HumanRequestChange{{Field: "autoResume", From: string(domain.AutoResumeNever), To: string(domain.AutoResumeAllowed)}},
	}, nil
}

func (autoResumeLift) apply(ctx context.Context, s *Service, _ domain.HumanRequest, target domain.SessionRecord) (string, error) {
	policy, err := s.autoResume.AutoResumePolicy(ctx, target.ID)
	if err != nil {
		return "", err
	}
	if policy.Allows() {
		return "automatic resume was already allowed", nil
	}
	// The person approved this in the app, so it is applied as the person's own change.
	if err := s.autoResume.SetAutoResumePolicy(ctx, target.ID, domain.AutoResumeAllowed, ""); err != nil {
		return "", err
	}
	return "automatic resume allowed", nil
}

// ---- conversation_settings ----

type conversationSettings struct{}

// settingsPayload is the whole vocabulary of this kind. A permission mode is deliberately absent:
// raising what an agent may do without asking is a different decision with its own kind, and the
// strict decoder refuses the field instead of ignoring it.
type settingsPayload struct {
	Model           *string `json:"model,omitempty"`
	ReasoningEffort *string `json:"reasoningEffort,omitempty"`
	FastMode        *bool   `json:"fastMode,omitempty"`
	PlanMode        *bool   `json:"planMode,omitempty"`
}

func boolText(b bool) string { return strconv.FormatBool(b) }

func modelText(model string) string {
	if model == "" {
		return "provider default"
	}
	return model
}

func effortText(effort string) string {
	if effort == "" {
		return "provider default"
	}
	return effort
}

func (conversationSettings) prepare(ctx context.Context, s *Service, target domain.SessionRecord, raw json.RawMessage) (prepared, error) {
	if s.chat == nil {
		return prepared{}, apierr.NotImplemented("HUMAN_REQUEST_KIND_UNAVAILABLE", "conversation settings are not available on this daemon")
	}
	var p settingsPayload
	if err := decodeStrict(raw, &p); err != nil {
		return prepared{}, err
	}
	if p.Model == nil && p.ReasoningEffort == nil && p.FastMode == nil && p.PlanMode == nil {
		return prepared{}, apierr.Invalid("HUMAN_REQUEST_PAYLOAD_INVALID", "name at least one setting to change: model, reasoningEffort, fastMode or planMode", nil)
	}
	if target.IsTerminated {
		return prepared{}, apierr.Invalid("HUMAN_REQUEST_SESSION_ENDED", "that session has ended", nil)
	}
	models, current, err := s.chat.Models(ctx, target.ID)
	if err != nil {
		if errors.Is(err, ports.ErrChatUnsupported) {
			return prepared{}, apierr.Invalid("HUMAN_REQUEST_NOT_CHAT", "that session is not a chat conversation", nil)
		}
		return prepared{}, err
	}
	next := current
	var changes []domain.HumanRequestChange
	var parts []string
	if p.Model != nil {
		model := strings.TrimSpace(*p.Model)
		if model == "" || !slices.ContainsFunc(models, func(m ports.ChatModel) bool { return m.ID == model }) {
			return prepared{}, apierr.Invalid("HUMAN_REQUEST_UNKNOWN_MODEL", fmt.Sprintf("%q is not one of the models this conversation offers", model), nil)
		}
		p.Model = &model
		next.Model = model
		changes = append(changes, domain.HumanRequestChange{Field: "model", From: modelText(current.Model), To: model})
		parts = append(parts, "model to "+model)
	}
	if p.ReasoningEffort != nil {
		effort := strings.TrimSpace(*p.ReasoningEffort)
		if !validToken(effort) {
			return prepared{}, apierr.Invalid("HUMAN_REQUEST_UNKNOWN_EFFORT", "name the reasoning effort as the model lists it (letters, digits and . _ : / -, at most 64)", nil)
		}
		p.ReasoningEffort = &effort
		next.ReasoningEffort = effort
		changes = append(changes, domain.HumanRequestChange{Field: "reasoningEffort", From: effortText(current.ReasoningEffort), To: effort})
		parts = append(parts, "reasoning effort to "+effort)
	}
	if err := checkEffort(models, next); err != nil {
		return prepared{}, err
	}
	if p.FastMode != nil {
		changes = append(changes, domain.HumanRequestChange{Field: "fastMode", From: boolText(current.FastMode), To: boolText(*p.FastMode)})
		parts = append(parts, "fast mode "+onOff(*p.FastMode))
	}
	if p.PlanMode != nil {
		changes = append(changes, domain.HumanRequestChange{Field: "planMode", From: boolText(current.PlanMode), To: boolText(*p.PlanMode)})
		parts = append(parts, "plan mode "+onOff(*p.PlanMode))
	}
	payload, err := canonical(p)
	if err != nil {
		return prepared{}, err
	}
	return prepared{payload: payload, summary: fmt.Sprintf("Change %s: %s.", sessionLabel(target), strings.Join(parts, ", ")), changes: changes}, nil
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// checkEffort refuses an effort the chosen model does not take, so approving never lands on a combination
// the provider would reject or silently change.
func checkEffort(models []ports.ChatModel, next domain.ConversationSettings) error {
	if next.ReasoningEffort == "" {
		return nil
	}
	for _, m := range models {
		if m.ID == next.Model || (next.Model == "" && m.Default) {
			if len(m.Efforts) > 0 && !slices.Contains(m.Efforts, next.ReasoningEffort) {
				return apierr.Invalid("HUMAN_REQUEST_UNKNOWN_EFFORT", fmt.Sprintf("%q is not a reasoning effort %s takes (%s)", next.ReasoningEffort, m.ID, strings.Join(m.Efforts, ", ")), nil)
			}
			return nil
		}
	}
	return nil
}

func (conversationSettings) apply(ctx context.Context, s *Service, req domain.HumanRequest, target domain.SessionRecord) (string, error) {
	var p settingsPayload
	if err := decodeStrict(req.Payload, &p); err != nil {
		return "", err
	}
	_, current, err := s.chat.Models(ctx, target.ID)
	if err != nil {
		return "", err
	}
	// The diff the person approved named the values it started from. If they moved, it no longer
	// describes this change, so nothing is applied.
	now := map[string]string{
		"model": modelText(current.Model), "reasoningEffort": effortText(current.ReasoningEffort),
		"fastMode": boolText(current.FastMode), "planMode": boolText(current.PlanMode),
	}
	for _, change := range req.Changes {
		if got, known := now[change.Field]; known && got != change.From {
			return "", staleError{reason: fmt.Sprintf("%s is now %s, not %s as when this was requested; ask again", change.Field, got, change.From)}
		}
	}
	next := current
	if p.Model != nil {
		next.Model = *p.Model
	}
	if p.ReasoningEffort != nil {
		next.ReasoningEffort = *p.ReasoningEffort
	}
	if p.FastMode != nil {
		next.FastMode = *p.FastMode
	}
	if p.PlanMode != nil {
		next.PlanMode = *p.PlanMode
	}
	if _, err := s.chat.SetTurnSettings(ctx, target.ID, next); err != nil {
		return "", err
	}
	return "settings updated", nil
}

// ---- provider_switch ----

type providerSwitch struct{}

type switchPayload struct {
	TargetHarness string  `json:"targetHarness"`
	Model         *string `json:"model,omitempty"`
	Effort        *string `json:"effort,omitempty"`
	FastMode      *bool   `json:"fastMode,omitempty"`
}

func (providerSwitch) prepare(_ context.Context, s *Service, target domain.SessionRecord, raw json.RawMessage) (prepared, error) {
	if s.chat == nil {
		return prepared{}, apierr.NotImplemented("HUMAN_REQUEST_KIND_UNAVAILABLE", "switching agents is not available on this daemon")
	}
	var p switchPayload
	if err := decodeStrict(raw, &p); err != nil {
		return prepared{}, err
	}
	harness := domain.AgentHarness(strings.TrimSpace(p.TargetHarness))
	if !harness.IsKnown() {
		return prepared{}, apierr.Invalid("HUMAN_REQUEST_UNKNOWN_HARNESS", fmt.Sprintf("%q is not a known agent", p.TargetHarness), nil)
	}
	if harness == target.Harness {
		return prepared{}, apierr.Conflict("HUMAN_REQUEST_NOT_NEEDED", "that session already runs on that agent", nil)
	}
	if target.IsTerminated {
		return prepared{}, apierr.Invalid("HUMAN_REQUEST_SESSION_ENDED", "that session has ended", nil)
	}
	p.TargetHarness = string(harness)
	changes := []domain.HumanRequestChange{{Field: "harness", From: string(target.Harness), To: string(harness)}}
	summary := fmt.Sprintf("Switch %s from %s to %s", sessionLabel(target), target.Harness, harness)
	var extras []string
	if p.Model != nil {
		model := strings.TrimSpace(*p.Model)
		if !validToken(model) {
			return prepared{}, apierr.Invalid("HUMAN_REQUEST_UNKNOWN_MODEL", "name the model by its id (letters, digits and . _ : / -, at most 64)", nil)
		}
		p.Model = &model
		changes = append(changes, domain.HumanRequestChange{Field: "model", From: "", To: model})
		extras = append(extras, "model "+model)
	}
	if p.Effort != nil {
		effort := strings.TrimSpace(*p.Effort)
		if !validToken(effort) {
			return prepared{}, apierr.Invalid("HUMAN_REQUEST_UNKNOWN_EFFORT", "name the reasoning effort by its id (letters, digits and . _ : / -, at most 64)", nil)
		}
		p.Effort = &effort
		changes = append(changes, domain.HumanRequestChange{Field: "reasoningEffort", From: "", To: effort})
		extras = append(extras, "reasoning effort "+effort)
	}
	if p.FastMode != nil {
		changes = append(changes, domain.HumanRequestChange{Field: "fastMode", From: "", To: boolText(*p.FastMode)})
		extras = append(extras, "fast mode "+onOff(*p.FastMode))
	}
	if len(extras) > 0 {
		summary += " (" + strings.Join(extras, ", ") + ")"
	}
	payload, err := canonical(p)
	if err != nil {
		return prepared{}, err
	}
	return prepared{payload: payload, summary: summary + ".", changes: changes}, nil
}

func (providerSwitch) apply(ctx context.Context, s *Service, req domain.HumanRequest, target domain.SessionRecord) (string, error) {
	var p switchPayload
	if err := decodeStrict(req.Payload, &p); err != nil {
		return "", err
	}
	for _, change := range req.Changes {
		if change.Field == "harness" && string(target.Harness) != change.From {
			return "", staleError{reason: fmt.Sprintf("the session now runs on %s, not %s as when this was requested; ask again", target.Harness, change.From)}
		}
	}
	request := domain.ProviderSwitchRequest{TargetHarness: domain.AgentHarness(p.TargetHarness), IdempotencyKey: "human-request-" + req.ID}
	if p.Model != nil {
		request.Model = *p.Model
	}
	if p.Effort != nil {
		request.Effort = *p.Effort
	}
	if p.FastMode != nil {
		request.FastMode = *p.FastMode
	}
	if err := s.chat.SwitchProvider(ctx, target.ID, request); err != nil {
		return "", err
	}
	return "switched to " + p.TargetHarness, nil
}
