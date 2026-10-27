package humanrequest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

type fakeAutoResume struct {
	mu      sync.Mutex
	policy  map[domain.SessionID]domain.AutoResumePolicy
	setCall []string
}

func (f *fakeAutoResume) AutoResumePolicy(_ context.Context, id domain.SessionID) (domain.AutoResumePolicy, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p, ok := f.policy[id]; ok {
		return p, nil
	}
	return domain.AutoResumeAllowed, nil
}

func (f *fakeAutoResume) SetAutoResumePolicy(_ context.Context, id domain.SessionID, policy domain.AutoResumePolicy, sender domain.SessionID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.policy[id] = policy
	f.setCall = append(f.setCall, fmt.Sprintf("%s=%s by %q", id, policy, sender))
	return nil
}

type fakeChat struct {
	mu        sync.Mutex
	models    []ports.ChatModel
	settings  domain.ConversationSettings
	setCalls  []domain.ConversationSettings
	switches  []domain.ProviderSwitchRequest
	switchErr error
}

func (f *fakeChat) Models(context.Context, domain.SessionID) ([]ports.ChatModel, domain.ConversationSettings, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.models, f.settings, nil
}

func (f *fakeChat) SetTurnSettings(_ context.Context, _ domain.SessionID, settings domain.ConversationSettings) (domain.ConversationSettings, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setCalls = append(f.setCalls, settings)
	f.settings = settings
	return settings, nil
}

func (f *fakeChat) SwitchProvider(_ context.Context, _ domain.SessionID, request domain.ProviderSwitchRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.switches = append(f.switches, request)
	return f.switchErr
}

type fakeMessenger struct {
	mu   sync.Mutex
	sent []string
}

func (f *fakeMessenger) Send(_ context.Context, id domain.SessionID, message string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, string(id)+": "+message)
	return nil
}

func (f *fakeMessenger) messages() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent...)
}

type fixture struct {
	svc    *Service
	store  *sqlite.Store
	ar     *fakeAutoResume
	chat   *fakeChat
	msgs   *fakeMessenger
	now    time.Time
	clock  func() time.Time
	nextID int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	store, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "repo", Path: "/tmp/repo", RegisteredAt: time.Now().UTC().Truncate(time.Second)}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Fix the bug", "Requester", "Other requester"} {
		now := time.Now().UTC().Truncate(time.Second)
		if _, err := store.CreateSession(ctx, domain.SessionRecord{
			ProjectID: "repo", Kind: domain.KindWorker, Harness: domain.HarnessClaudeCode, DisplayName: name,
			Activity: domain.Activity{State: domain.ActivityActive, LastActivityAt: now},
			Metadata: domain.SessionMetadata{Branch: "feat/x", WorkspacePath: "/ws"}, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	f := &fixture{
		store: store,
		ar:    &fakeAutoResume{policy: map[domain.SessionID]domain.AutoResumePolicy{"repo-1": domain.AutoResumeNever}},
		chat: &fakeChat{
			models: []ports.ChatModel{
				{ID: "m-a", Default: true, Efforts: []string{"low", "high"}},
				{ID: "m-b", Efforts: []string{"low", "medium"}},
			},
			settings: domain.ConversationSettings{Model: "m-a", ReasoningEffort: "low", ApprovalMode: domain.PermissionModeDefault},
		},
		msgs: &fakeMessenger{},
		now:  time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC),
	}
	f.clock = func() time.Time { return f.now }
	f.svc = New(Config{
		Store: store, AutoResume: f.ar, Chat: f.chat, Messenger: f.msgs, Now: f.clock,
		NewID: func() string { f.nextID++; return fmt.Sprintf("hr-%d", f.nextID) },
	})
	return f
}

// The sessions are repo-1 (the target, "Fix the bug"), repo-2 and repo-3 (requesters).
const (
	target    = domain.SessionID("repo-1")
	agent     = domain.SessionID("repo-2")
	otherType = domain.SessionID("repo-3")
)

func agentRequester(id domain.SessionID) domain.HumanRequestRequester {
	return domain.HumanRequestRequester{Kind: domain.HumanRequesterSession, SessionID: id}
}

func (f *fixture) file(t *testing.T, kind domain.HumanRequestKind, payload string) domain.HumanRequest {
	t.Helper()
	req, err := f.svc.File(context.Background(), FileInput{Kind: kind, SessionID: target, Payload: json.RawMessage(payload), Requester: agentRequester(agent)})
	if err != nil {
		t.Fatalf("file %s %s: %v", kind, payload, err)
	}
	return req
}

func codeOf(t *testing.T, err error) string {
	t.Helper()
	var apiErr *apierr.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("error %v is not an apierr", err)
	}
	return apiErr.Code
}

func TestFilingALiftRendersTheSummaryAndDiffFromTheStateAndChangesNothing(t *testing.T) {
	f := newFixture(t)
	req := f.file(t, domain.HumanRequestAutoResumeLift, `{}`)
	if req.State != domain.HumanRequestPending || req.PayloadHash == "" || !req.ExpiresAt.Equal(f.now.Add(RequestTTL)) {
		t.Fatalf("request = %+v", req)
	}
	if !strings.Contains(req.Summary, "Fix the bug (repo-1)") || !strings.Contains(req.Summary, "will not resume it, replace it or tear it down") {
		t.Fatalf("summary does not name the session and what allowing means: %q", req.Summary)
	}
	if len(req.Changes) != 1 || req.Changes[0] != (domain.HumanRequestChange{Field: "autoResume", From: "never", To: "allowed"}) {
		t.Fatalf("changes = %+v", req.Changes)
	}
	if req.Requester.SessionID != agent || len(f.ar.setCall) != 0 || len(f.msgs.messages()) != 0 {
		t.Fatalf("filing must not change the policy or message anyone: requester %+v, sets %v, messages %v", req.Requester, f.ar.setCall, f.msgs.messages())
	}
	again := f.file(t, domain.HumanRequestAutoResumeLift, `{}`)
	if again.ID != req.ID {
		t.Fatalf("a repeated filing made %s, want the open %s", again.ID, req.ID)
	}
}

func TestTheRequesterCannotInfluenceTheRenderedText(t *testing.T) {
	f := newFixture(t)
	for _, payload := range []string{`{"summary":"fix the build"}`, `{"note":"x"}`, `[]`, `{} {}`} {
		if _, err := f.svc.File(context.Background(), FileInput{Kind: domain.HumanRequestAutoResumeLift, SessionID: target, Payload: json.RawMessage(payload), Requester: agentRequester(agent)}); err == nil {
			t.Errorf("payload %s was accepted: requester text must have nowhere to go", payload)
		} else if code := codeOf(t, err); code != "HUMAN_REQUEST_PAYLOAD_INVALID" {
			t.Errorf("payload %s: code %s", payload, code)
		}
	}
}

func TestApprovingNeedsTheHashOfWhatWasShownAndAppliesOnce(t *testing.T) {
	f := newFixture(t)
	req := f.file(t, domain.HumanRequestAutoResumeLift, `{}`)
	ctx := context.Background()

	for _, wrong := range []string{"", "nope", req.PayloadHash + "0", strings.ToUpper(req.PayloadHash)} {
		if _, err := f.svc.Approve(ctx, req.ID, wrong); err == nil || codeOf(t, err) != "HUMAN_REQUEST_CHANGED" {
			t.Fatalf("hash %q: err = %v, want HUMAN_REQUEST_CHANGED", wrong, err)
		}
	}
	if len(f.ar.setCall) != 0 {
		t.Fatalf("a wrong hash applied the change: %v", f.ar.setCall)
	}

	done, err := f.svc.Approve(ctx, req.ID, req.PayloadHash)
	if err != nil || done.State != domain.HumanRequestApplied || done.Result == "" {
		t.Fatalf("approve = %+v, %v", done, err)
	}
	if len(f.ar.setCall) != 1 || f.ar.setCall[0] != `repo-1=allowed by ""` {
		t.Fatalf("applied as %v", f.ar.setCall)
	}
	if msgs := f.msgs.messages(); len(msgs) != 1 || !strings.HasPrefix(msgs[0], "repo-2: [AO] Your request "+req.ID) || !strings.Contains(msgs[0], "approved and applied") {
		t.Fatalf("requester was told %v", msgs)
	}
	again, err := f.svc.Approve(ctx, req.ID, req.PayloadHash)
	if err != nil || again.State != domain.HumanRequestApplied || len(f.ar.setCall) != 1 || len(f.msgs.messages()) != 1 {
		t.Fatalf("a repeated approve = %+v, %v; sets %v, messages %v", again, err, f.ar.setCall, f.msgs.messages())
	}
}

func TestConcurrentApprovalsApplyExactlyOnce(t *testing.T) {
	f := newFixture(t)
	req := f.file(t, domain.HumanRequestAutoResumeLift, `{}`)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = f.svc.Approve(context.Background(), req.ID, req.PayloadHash)
		}()
	}
	wg.Wait()
	if len(f.ar.setCall) != 1 {
		t.Fatalf("the change was applied %d times: %v", len(f.ar.setCall), f.ar.setCall)
	}
}

func TestDenyingTellsTheRequesterNotToRetryAndChangesNothing(t *testing.T) {
	f := newFixture(t)
	req := f.file(t, domain.HumanRequestAutoResumeLift, `{}`)
	ctx := context.Background()
	denied, err := f.svc.Deny(ctx, req.ID)
	if err != nil || denied.State != domain.HumanRequestDenied {
		t.Fatalf("deny = %+v, %v", denied, err)
	}
	if len(f.ar.setCall) != 0 {
		t.Fatalf("denying changed the policy: %v", f.ar.setCall)
	}
	if msgs := f.msgs.messages(); len(msgs) != 1 || !strings.Contains(msgs[0], "was denied") || !strings.Contains(msgs[0], "Do not file it again") {
		t.Fatalf("requester was told %v", msgs)
	}
	if again, err := f.svc.Deny(ctx, req.ID); err != nil || again.State != domain.HumanRequestDenied || len(f.msgs.messages()) != 1 {
		t.Fatalf("a repeated deny = %+v, %v, messages %v", again, err, f.msgs.messages())
	}
	if _, err := f.svc.Approve(ctx, req.ID, req.PayloadHash); err == nil || codeOf(t, err) != "HUMAN_REQUEST_DECIDED" {
		t.Fatalf("approving a denied request: %v", err)
	}
	// A denied request can be asked for again, as a new card.
	if next := f.file(t, domain.HumanRequestAutoResumeLift, `{}`); next.ID == req.ID {
		t.Fatal("a denied request was reused")
	}
}

func TestAnExpiredRequestIsSweptTheRequesterToldAndApprovalRefused(t *testing.T) {
	f := newFixture(t)
	req := f.file(t, domain.HumanRequestAutoResumeLift, `{}`)
	f.now = f.now.Add(RequestTTL + time.Minute)
	open, err := f.svc.List(context.Background(), false)
	if err != nil || len(open) != 0 {
		t.Fatalf("open after the deadline = %+v, %v", open, err)
	}
	if msgs := f.msgs.messages(); len(msgs) != 1 || !strings.Contains(msgs[0], "expired before anyone decided") {
		t.Fatalf("requester was told %v", msgs)
	}
	if _, err := f.svc.Approve(context.Background(), req.ID, req.PayloadHash); err == nil || codeOf(t, err) != "HUMAN_REQUEST_EXPIRED" {
		t.Fatalf("approve after expiry: %v", err)
	}
	if len(f.ar.setCall) != 0 {
		t.Fatalf("an expired request was applied: %v", f.ar.setCall)
	}
	if recent, _ := f.svc.List(context.Background(), true); len(recent) != 1 || recent[0].State != domain.HumanRequestExpired {
		t.Fatalf("recent = %+v", recent)
	}
}

func TestFilingIsRefusedForUnknownThingsAndNeedlessAsks(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	cases := []struct {
		name string
		in   FileInput
		code string
	}{
		{"unknown kind", FileInput{Kind: "format_disk", SessionID: target, Requester: agentRequester(agent)}, "HUMAN_REQUEST_KIND_UNKNOWN"},
		{"unknown session", FileInput{Kind: domain.HumanRequestAutoResumeLift, SessionID: "nope-9", Requester: agentRequester(agent)}, "SESSION_NOT_FOUND"},
		{"requester that does not exist", FileInput{Kind: domain.HumanRequestAutoResumeLift, SessionID: target, Requester: agentRequester("ghost-1")}, "HUMAN_REQUEST_REQUESTER_UNKNOWN"},
		{"already allowed", FileInput{Kind: domain.HumanRequestAutoResumeLift, SessionID: agent, Requester: agentRequester(agent)}, "HUMAN_REQUEST_NOT_NEEDED"},
	}
	for _, tc := range cases {
		if _, err := f.svc.File(ctx, tc.in); err == nil || codeOf(t, err) != tc.code {
			t.Errorf("%s: err = %v, want %s", tc.name, err, tc.code)
		}
	}
	// A caller that names no session is a terminal with no identity: allowed to ask, never told anything.
	req, err := f.svc.File(ctx, FileInput{Kind: domain.HumanRequestAutoResumeLift, SessionID: target, Requester: domain.HumanRequestRequester{Kind: domain.HumanRequesterSession}})
	if err != nil || req.Requester.Kind != domain.HumanRequesterTerminal || req.Requester.SessionID != "" {
		t.Fatalf("anonymous request = %+v, %v", req, err)
	}
	if _, err := f.svc.Deny(ctx, req.ID); err != nil || len(f.msgs.messages()) != 0 {
		t.Fatalf("deny a terminal's request: %v, messages %v", err, f.msgs.messages())
	}
}

func TestTheOpenCapRefusesTheSixthRequestButStillAnswersARetryOfAnOpenOne(t *testing.T) {
	f := newFixture(t)
	f.chat.models = []ports.ChatModel{{ID: "m-1"}, {ID: "m-2"}, {ID: "m-3"}, {ID: "m-4"}, {ID: "m-5"}, {ID: "m-6"}}
	f.chat.settings = domain.ConversationSettings{}
	var first domain.HumanRequest
	for i := 1; i <= MaxOpenPerRequester; i++ {
		req := f.file(t, domain.HumanRequestConversationSettings, fmt.Sprintf(`{"model":"m-%d"}`, i))
		if i == 1 {
			first = req
		}
	}
	_, err := f.svc.File(context.Background(), FileInput{Kind: domain.HumanRequestConversationSettings, SessionID: target, Payload: json.RawMessage(`{"model":"m-6"}`), Requester: agentRequester(agent)})
	if err == nil || codeOf(t, err) != "HUMAN_REQUEST_LIMIT" {
		t.Fatalf("sixth request: %v", err)
	}
	retry, err := f.svc.File(context.Background(), FileInput{Kind: domain.HumanRequestConversationSettings, SessionID: target, Payload: json.RawMessage(`{"model":"m-1"}`), Requester: agentRequester(agent)})
	if err != nil || retry.ID != first.ID {
		t.Fatalf("a retry of an open request at the cap = %+v, %v; want %s back", retry, err, first.ID)
	}
	other, err := f.svc.File(context.Background(), FileInput{Kind: domain.HumanRequestConversationSettings, SessionID: target, Payload: json.RawMessage(`{"model":"m-6"}`), Requester: agentRequester(otherType)})
	if err != nil || other.Requester.SessionID != otherType {
		t.Fatalf("another requester is not limited by the first: %+v, %v", other, err)
	}
}

func TestSettingsRequestsRenderTheDiffAndApplyOnlyWhatWasShown(t *testing.T) {
	f := newFixture(t)
	req := f.file(t, domain.HumanRequestConversationSettings, `{"model":"m-b","reasoningEffort":"medium","planMode":true}`)
	if req.Summary != "Change Fix the bug (repo-1): model to m-b, reasoning effort to medium, plan mode on." {
		t.Fatalf("summary = %q", req.Summary)
	}
	want := []domain.HumanRequestChange{
		{Field: "model", From: "m-a", To: "m-b"},
		{Field: "reasoningEffort", From: "low", To: "medium"},
		{Field: "planMode", From: "false", To: "true"},
	}
	if fmt.Sprint(req.Changes) != fmt.Sprint(want) {
		t.Fatalf("changes = %+v, want %+v", req.Changes, want)
	}
	if len(f.chat.setCalls) != 0 {
		t.Fatal("filing changed the settings")
	}
	if _, err := f.svc.Approve(context.Background(), req.ID, req.PayloadHash); err != nil {
		t.Fatal(err)
	}
	if len(f.chat.setCalls) != 1 {
		t.Fatalf("settings applied %d times", len(f.chat.setCalls))
	}
	got := f.chat.setCalls[0]
	if got.Model != "m-b" || got.ReasoningEffort != "medium" || !got.PlanMode || got.FastMode || got.ApprovalMode != domain.PermissionModeDefault {
		t.Fatalf("applied settings = %+v (the permission mode must be left exactly as it was)", got)
	}
}

func TestSettingsRequestsRefuseWhatTheConversationDoesNotOfferAndAnyPermissionMode(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for name, tc := range map[string]struct{ payload, code string }{
		"unknown model":            {`{"model":"gpt-9000"}`, "HUMAN_REQUEST_UNKNOWN_MODEL"},
		"empty model":              {`{"model":" "}`, "HUMAN_REQUEST_UNKNOWN_MODEL"},
		"effort the model lacks":   {`{"model":"m-b","reasoningEffort":"high"}`, "HUMAN_REQUEST_UNKNOWN_EFFORT"},
		"effort lacking on a keep": {`{"reasoningEffort":"medium"}`, "HUMAN_REQUEST_UNKNOWN_EFFORT"},
		"empty effort":             {`{"reasoningEffort":""}`, "HUMAN_REQUEST_UNKNOWN_EFFORT"},
		"nothing to change":        {`{}`, "HUMAN_REQUEST_PAYLOAD_INVALID"},
		"permission mode":          {`{"approvalMode":"bypass-permissions"}`, "HUMAN_REQUEST_PAYLOAD_INVALID"},
		"permission mode + model":  {`{"model":"m-b","approvalMode":"bypass-permissions"}`, "HUMAN_REQUEST_PAYLOAD_INVALID"},
		"unknown field":            {`{"model":"m-b","autoCompactWindow":"1m"}`, "HUMAN_REQUEST_PAYLOAD_INVALID"},
	} {
		_, err := f.svc.File(ctx, FileInput{Kind: domain.HumanRequestConversationSettings, SessionID: target, Payload: json.RawMessage(tc.payload), Requester: agentRequester(agent)})
		if err == nil || codeOf(t, err) != tc.code {
			t.Errorf("%s: err = %v, want %s", name, err, tc.code)
		}
	}
}

func TestASettingsRequestFailsAsStaleWhenTheValuesMovedAfterFiling(t *testing.T) {
	f := newFixture(t)
	req := f.file(t, domain.HumanRequestConversationSettings, `{"model":"m-b","reasoningEffort":"medium"}`)
	f.chat.settings.Model = "m-b" // someone changed it in the meantime
	_, err := f.svc.Approve(context.Background(), req.ID, req.PayloadHash)
	if err == nil || codeOf(t, err) != "HUMAN_REQUEST_STALE" {
		t.Fatalf("approve after the model moved: %v", err)
	}
	if len(f.chat.setCalls) != 0 {
		t.Fatalf("a stale request was applied: %+v", f.chat.setCalls)
	}
	got, _ := f.svc.Get(context.Background(), req.ID)
	if got.State != domain.HumanRequestFailed || !strings.Contains(got.Result, "ask again") {
		t.Fatalf("request = %+v", got)
	}
	if msgs := f.msgs.messages(); len(msgs) != 1 || !strings.Contains(msgs[0], "could not be applied") {
		t.Fatalf("requester was told %v", msgs)
	}
}

func TestSettingsFiledAgainAfterTheValuesMovedMakeANewCard(t *testing.T) {
	f := newFixture(t)
	first := f.file(t, domain.HumanRequestConversationSettings, `{"model":"m-b","reasoningEffort":"medium"}`)
	f.chat.settings.Model = "m-b"
	second := f.file(t, domain.HumanRequestConversationSettings, `{"model":"m-b","reasoningEffort":"medium"}`)
	if first.ID == second.ID || first.PayloadHash == second.PayloadHash {
		t.Fatal("a request whose diff differs shared the earlier card's hash")
	}
}

func TestProviderSwitchRendersApprovesAndAppliesThroughTheSwitchService(t *testing.T) {
	f := newFixture(t)
	req := f.file(t, domain.HumanRequestProviderSwitch, `{"targetHarness":"codex","model":"o-big","effort":"high"}`)
	if !strings.Contains(req.Summary, "Switch Fix the bug (repo-1) from claude-code to codex (model o-big, reasoning effort high).") {
		t.Fatalf("summary = %q", req.Summary)
	}
	if req.Changes[0] != (domain.HumanRequestChange{Field: "harness", From: "claude-code", To: "codex"}) {
		t.Fatalf("changes = %+v", req.Changes)
	}
	if _, err := f.svc.Approve(context.Background(), req.ID, req.PayloadHash); err != nil {
		t.Fatal(err)
	}
	if len(f.chat.switches) != 1 {
		t.Fatalf("switched %d times", len(f.chat.switches))
	}
	got := f.chat.switches[0]
	if got.TargetHarness != domain.HarnessCodex || got.Model != "o-big" || got.Effort != "high" || got.IdempotencyKey != "human-request-"+req.ID || got.PermissionMode != "" {
		t.Fatalf("switch request = %+v (the permission mode must stay unset)", got)
	}
}

func TestProviderSwitchRefusesTheSameOrAnUnknownHarnessAndRecordsAFailedApply(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for name, tc := range map[string]struct{ payload, code string }{
		"same harness":    {`{"targetHarness":"claude-code"}`, "HUMAN_REQUEST_NOT_NEEDED"},
		"unknown harness": {`{"targetHarness":"skynet"}`, "HUMAN_REQUEST_UNKNOWN_HARNESS"},
		"no harness":      {`{}`, "HUMAN_REQUEST_UNKNOWN_HARNESS"},
		"permission mode": {`{"targetHarness":"codex","permissionMode":"bypass-permissions"}`, "HUMAN_REQUEST_PAYLOAD_INVALID"},
	} {
		_, err := f.svc.File(ctx, FileInput{Kind: domain.HumanRequestProviderSwitch, SessionID: target, Payload: json.RawMessage(tc.payload), Requester: agentRequester(agent)})
		if err == nil || codeOf(t, err) != tc.code {
			t.Errorf("%s: err = %v, want %s", name, err, tc.code)
		}
	}
	req := f.file(t, domain.HumanRequestProviderSwitch, `{"targetHarness":"codex"}`)
	f.chat.switchErr = errors.New("the target agent is not installed")
	_, err := f.svc.Approve(ctx, req.ID, req.PayloadHash)
	if err == nil || codeOf(t, err) != "HUMAN_REQUEST_FAILED" || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("approve with a failing switch: %v", err)
	}
	got, _ := f.svc.Get(ctx, req.ID)
	if got.State != domain.HumanRequestFailed || !strings.Contains(got.Result, "not installed") {
		t.Fatalf("request = %+v", got)
	}
	if msgs := f.msgs.messages(); len(msgs) != 1 || !strings.Contains(msgs[0], "could not be applied: the target agent is not installed") {
		t.Fatalf("requester was told %v", msgs)
	}
}

func TestProviderSwitchFailsAsStaleWhenTheHarnessChangedAfterFiling(t *testing.T) {
	f := newFixture(t)
	req := f.file(t, domain.HumanRequestProviderSwitch, `{"targetHarness":"codex"}`)
	rec, _, _ := f.store.GetSession(context.Background(), target)
	rec.Harness = domain.HarnessCodex
	if err := f.store.UpdateSession(context.Background(), rec); err != nil {
		t.Skipf("cannot move the session's harness in this store: %v", err)
	}
	_, err := f.svc.Approve(context.Background(), req.ID, req.PayloadHash)
	if err == nil || codeOf(t, err) != "HUMAN_REQUEST_STALE" || len(f.chat.switches) != 0 {
		t.Fatalf("approve after the harness moved: %v (switches %d)", err, len(f.chat.switches))
	}
}

func TestAKindWhoseServiceIsMissingIsRefusedInsteadOfSilentlyDoingNothing(t *testing.T) {
	f := newFixture(t)
	bare := New(Config{Store: f.store, Now: f.clock, NewID: func() string { return "hr-x" }})
	for _, kind := range KnownKinds() {
		if _, err := bare.File(context.Background(), FileInput{Kind: kind, SessionID: target, Payload: json.RawMessage(`{}`), Requester: agentRequester(agent)}); err == nil || codeOf(t, err) != "HUMAN_REQUEST_KIND_UNAVAILABLE" {
			t.Errorf("%s without its service: %v", kind, err)
		}
	}
}

func TestKnownKindsAreTheThreeThisPieceShips(t *testing.T) {
	if got := fmt.Sprint(KnownKinds()); got != "[auto_resume_lift conversation_settings provider_switch]" {
		t.Fatalf("kinds = %s", got)
	}
}
