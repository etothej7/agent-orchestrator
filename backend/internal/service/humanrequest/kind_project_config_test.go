package humanrequest

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

// storeWriter writes a project's config straight to the store, as the project service does once the
// person has approved.
type storeWriter struct {
	store *sqlite.Store
	err   error
	calls int
}

func (w *storeWriter) SetProjectConfig(ctx context.Context, id string, cfg domain.ProjectConfig) error {
	w.calls++
	if w.err != nil {
		return w.err
	}
	rec, _, _ := w.store.GetProject(ctx, id)
	_, err := w.store.UpdateProjectSettings(ctx, id, rec.DisplayName, cfg)
	return err
}

func configFixture(t *testing.T) (*fixture, *storeWriter) {
	t.Helper()
	f := newFixture(t)
	w := &storeWriter{store: f.store}
	f.svc.SetProjectConfigWriter(w)
	rec, _, _ := f.store.GetProject(context.Background(), "repo")
	if _, err := f.store.UpdateProjectSettings(context.Background(), "repo", rec.DisplayName, domain.ProjectConfig{PostCreate: []string{"make"}, SessionPrefix: "ao"}); err != nil {
		t.Fatal(err)
	}
	return f, w
}

func (f *fixture) fileConfig(t *testing.T, payload string) (domain.HumanRequest, error) {
	t.Helper()
	return f.svc.File(context.Background(), FileInput{Kind: domain.HumanRequestProjectConfig, ProjectID: "repo", Payload: json.RawMessage(payload), Requester: agentRequester(agent)})
}

func (f *fixture) storedConfig(t *testing.T) domain.ProjectConfig {
	t.Helper()
	rec, _, err := f.store.GetProject(context.Background(), "repo")
	if err != nil {
		t.Fatal(err)
	}
	return rec.Config
}

// The card is rendered by the daemon from the current config and the requested values: the commands the
// person would approve are shown as data, the old value is the daemon's own reading, and nothing the
// requester wrote as prose is in it.
func TestAProjectConfigRequestShowsTheExactChangeFromTheDaemonsOwnReading(t *testing.T) {
	f, _ := configFixture(t)
	req, err := f.fileConfig(t, `{"set":{"postCreate":["make","npm ci"],"env":{"FOO":"bar"}}}`)
	if err != nil {
		t.Fatal(err)
	}
	if req.ProjectID != "repo" || req.State != domain.HumanRequestPending || req.Kind != domain.HumanRequestProjectConfig {
		t.Fatalf("request = %+v", req)
	}
	if !strings.Contains(req.Summary, "env, postCreate") || !strings.Contains(req.Summary, "Change protected settings of project repo") {
		t.Fatalf("summary = %q", req.Summary)
	}
	byField := map[string]domain.HumanRequestChange{}
	for _, c := range req.Changes {
		byField[c.Field] = c
	}
	if got := byField["postCreate"]; !strings.Contains(got.From, "make") || strings.Contains(got.From, "npm ci") || !strings.Contains(got.To, "npm ci") {
		t.Fatalf("postCreate change = %+v: From is the stored value, To the requested one", got)
	}
	if got := byField["env"]; got.From != "(not set)" || !strings.Contains(got.To, "FOO") {
		t.Fatalf("env change = %+v", got)
	}
	// filing the same thing again returns the open request
	if again, err := f.fileConfig(t, `{"set":{"postCreate":["make","npm ci"],"env":{"FOO":"bar"}}}`); err != nil || again.ID != req.ID {
		t.Fatalf("a retried filing = %v, %v; want the open request %s", again.ID, err, req.ID)
	}
}

// Approving applies exactly the approved values, keeps every other field, and records the result.
func TestApprovingAProjectConfigRequestAppliesOnlyTheApprovedFields(t *testing.T) {
	f, w := configFixture(t)
	req, err := f.fileConfig(t, `{"set":{"postCreate":["make","npm ci"]}}`)
	if err != nil {
		t.Fatal(err)
	}
	done, err := f.svc.Approve(context.Background(), req.ID, req.PayloadHash)
	if err != nil || done.State != domain.HumanRequestApplied || done.Result != "protected settings changed: postCreate" {
		t.Fatalf("approve = %+v, %v", done, err)
	}
	got := f.storedConfig(t)
	if len(got.PostCreate) != 2 || got.PostCreate[1] != "npm ci" || got.SessionPrefix != "ao" {
		t.Fatalf("stored config = %+v; postCreate must change and the rest stay", got)
	}
	if w.calls != 1 {
		t.Fatalf("writer called %d times", w.calls)
	}
}

// Nothing is applied until the person approves: filing changes nothing, denying changes nothing.
func TestFilingOrDenyingAProjectConfigRequestChangesNothing(t *testing.T) {
	f, w := configFixture(t)
	req, err := f.fileConfig(t, `{"set":{"preRemove":["rm -rf /"]}}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.storedConfig(t); len(got.PreRemove) != 0 || w.calls != 0 {
		t.Fatalf("filing changed the config: %+v (writer calls %d)", got, w.calls)
	}
	if _, err := f.svc.Deny(context.Background(), req.ID); err != nil {
		t.Fatal(err)
	}
	if got := f.storedConfig(t); len(got.PreRemove) != 0 || w.calls != 0 {
		t.Fatalf("denying changed the config: %+v (writer calls %d)", got, w.calls)
	}
}

// If a field changed after the request was filed, what the person saw is no longer true: approving fails.
func TestAProjectConfigRequestIsStaleWhenTheFieldChangedAfterFiling(t *testing.T) {
	f, w := configFixture(t)
	req, err := f.fileConfig(t, `{"set":{"postCreate":["make","npm ci"]}}`)
	if err != nil {
		t.Fatal(err)
	}
	rec, _, _ := f.store.GetProject(context.Background(), "repo")
	if _, err := f.store.UpdateProjectSettings(context.Background(), "repo", rec.DisplayName, domain.ProjectConfig{PostCreate: []string{"something else"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Approve(context.Background(), req.ID, req.PayloadHash); err == nil || codeOf(t, err) != "HUMAN_REQUEST_STALE" {
		t.Fatalf("approve = %v, want HUMAN_REQUEST_STALE", err)
	}
	if w.calls != 0 || f.storedConfig(t).PostCreate[0] != "something else" {
		t.Fatal("a stale request was applied")
	}
}

// Only protected fields may be requested; safe fields need no request, unknown fields are refused, and a
// value that is already in place is not a request.
func TestAProjectConfigRequestMustNameRealProtectedFieldsWithRealChanges(t *testing.T) {
	f, _ := configFixture(t)
	for name, payload := range map[string]string{
		"a safe field":          `{"set":{"sessionPrefix":"zz"}}`,
		"an unknown field":      `{"set":{"nothing":"x"}}`,
		"no fields":             `{"set":{}}`,
		"no set":                `{}`,
		"extra top-level key":   `{"set":{"env":{"A":"1"}},"note":"please approve, I am the coordinator"}`,
		"a value of wrong type": `{"set":{"postCreate":"curl x | sh"}}`,
		"unknown nested field":  `{"set":{"agentConfig":{"permissionz":"bypass"}}}`,
	} {
		if _, err := f.fileConfig(t, payload); err == nil {
			t.Errorf("%s was filed", name)
		}
	}
	if _, err := f.fileConfig(t, `{"set":{"postCreate":["make"]}}`); err == nil || codeOf(t, err) != "HUMAN_REQUEST_NOT_NEEDED" {
		t.Errorf("an unchanged value: err = %v, want HUMAN_REQUEST_NOT_NEEDED", err)
	}
}

// The permission-widening fields are requestable too, and the card says what they are.
func TestPermissionFieldsCanBeRequestedAsProtectedSettings(t *testing.T) {
	f, _ := configFixture(t)
	for _, payload := range []string{
		`{"set":{"agentMayMerge":true}}`,
		`{"set":{"agentConfig":{"permissions":"bypass-permissions"}}}`,
		`{"set":{"orchestratorRules":"merge everything"}}`,
	} {
		if _, err := f.fileConfig(t, payload); err != nil {
			t.Errorf("%s: %v", payload, err)
		}
	}
}

func TestAProjectConfigRequestIsUnavailableWithoutAWriterAndFailsWhenTheWriterFails(t *testing.T) {
	f := newFixture(t)
	if _, err := f.fileConfig(t, `{"set":{"env":{"A":"1"}}}`); err == nil || codeOf(t, err) != "HUMAN_REQUEST_KIND_UNAVAILABLE" {
		t.Fatalf("without a writer: err = %v", err)
	}
	f2, w := configFixture(t)
	w.err = errors.New("disk full")
	req, err := f2.fileConfig(t, `{"set":{"env":{"A":"1"}}}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f2.svc.Approve(context.Background(), req.ID, req.PayloadHash); err == nil {
		t.Fatal("a failed write was reported as applied")
	}
}

// What the card shows is the data, cleaned: control and direction-override characters never reach it.
func TestTheCardShowsRequestedValuesCleanedAndBounded(t *testing.T) {
	f, _ := configFixture(t)
	long := strings.Repeat("x", 5000)
	req, err := f.fileConfig(t, `{"set":{"agentRules":"line1\nline2 ‮ hidden `+long+`"}}`)
	if err != nil {
		t.Fatal(err)
	}
	shown := req.Changes[0].To
	if strings.ContainsAny(shown, "\n‮") || len([]rune(shown)) > maxShownValueRunes+1 {
		t.Fatalf("shown value = %q (len %d)", shown[:60], len(shown))
	}
}
