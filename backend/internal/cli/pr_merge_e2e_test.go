package cli

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
	"github.com/aoagents/agent-orchestrator/backend/internal/identity"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/runfile"
	humanrequestsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/humanrequest"
	prsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/pr"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

const e2eHead = "0123456789abcdef0123456789abcdef01234567"

// e2eMerger stands in for the SCM provider: it records what it was asked to merge.
type e2eMerger struct {
	mu     sync.Mutex
	merged []prsvc.MergeRequest
}

func (m *e2eMerger) Merge(_ context.Context, r prsvc.MergeRequest) (prsvc.MergeResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.merged = append(m.merged, r)
	return prsvc.MergeResult{PRNumber: 7, Method: "squash", State: domain.PRStateMerged}, nil
}

func (m *e2eMerger) ResolveComments(context.Context, string, []string) (prsvc.ResolveResult, error) {
	return prsvc.ResolveResult{}, nil
}

func (m *e2eMerger) calls() []prsvc.MergeRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]prsvc.MergeRequest(nil), m.merged...)
}

type mergeRig struct {
	store  *sqlite.Store
	merger *e2eMerger
	svc    *humanrequestsvc.Service
	worker domain.SessionID
	proof  string
}

// newMergeRig starts the real daemon router (real store, real identity resolver, real pending-request service, the
// real merge route and controller) around a fake SCM, with a worker session that owns tracked pull request #7, and
// points the CLI at it.
func newMergeRig(t *testing.T, agentMayMerge bool) *mergeRig {
	t.Helper()
	ctx := context.Background()
	store := sqlitetest.MustOpen(t)
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "demo", Path: t.TempDir(), RegisteredAt: now, Config: domain.ProjectConfig{AgentMayMerge: agentMayMerge}}); err != nil {
		t.Fatal(err)
	}
	rec, err := store.CreateSession(ctx, domain.SessionRecord{
		ProjectID: "demo", Kind: domain.KindWorker, Harness: domain.HarnessCodex, DisplayName: "Fix the bug",
		Activity: domain.Activity{State: domain.ActivityActive, LastActivityAt: now}, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteSCMObservation(ctx, domain.PullRequest{
		URL: "https://github.com/acme/repo/pull/7", SessionID: rec.ID, Number: 7, HeadSHA: e2eHead, Title: "Fix the bug", UpdatedAt: now,
	}, nil, nil, nil, nil, ports.ReviewWritePreserve); err != nil {
		t.Fatal(err)
	}
	proof, err := identity.NewIssuer(store, nil).Issue(ctx, rec.ID)
	if err != nil {
		t.Fatal(err)
	}

	merger := &e2eMerger{}
	svc := humanrequestsvc.New(humanrequestsvc.Config{Store: store, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	svc.SetPRMerger(merger)
	router := httpd.NewRouterWithControl(config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, httpd.APIDeps{
		PRs:             merger,
		PRMergeStore:    store,
		HumanRequests:   svc,
		SessionIdentity: identity.NewResolver(store, store),
	}, httpd.ControlDeps{})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)

	runFile := filepath.Join(t.TempDir(), "running.json")
	t.Setenv("AO_RUN_FILE", runFile)
	if err := runfile.Write(runFile, runfile.Info{PID: os.Getpid(), Port: srv.Listener.Addr().(*net.TCPAddr).Port, StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	return &mergeRig{store: store, merger: merger, svc: svc, worker: rec.ID, proof: proof}
}

// asSession runs the CLI the way an agent's shell does: with the session's id and its identity proof in the environment.
func (r *mergeRig) asSession(t *testing.T) {
	t.Helper()
	t.Setenv("AO_SESSION_ID", string(r.worker))
	t.Setenv(identity.EnvProof, r.proof)
}

func (r *mergeRig) asTerminal(t *testing.T) {
	t.Helper()
	t.Setenv("AO_SESSION_ID", "")
	t.Setenv(identity.EnvProof, "")
}

func TestE2E_AnAgentMergeOnAProjectThatHasNotAllowedItFilesARequestThePersonCanApprove(t *testing.T) {
	rig := newMergeRig(t, false)
	rig.asSession(t)

	out, errOut, err := executeCLI(t, aliveDeps(), "pr", "merge", "7")
	if err != nil {
		t.Fatalf("ao pr merge 7: %v\nstderr=%s", err, errOut)
	}
	if strings.Contains(out, "merged PR") || !strings.Contains(out, "does not let agents merge") || !strings.Contains(out, "Do not retry") {
		t.Fatalf("stdout = %q; the agent must be told a request was filed, not that it merged", out)
	}
	if got := rig.merger.calls(); len(got) != 0 {
		t.Fatalf("the agent's merge reached the provider: %+v", got)
	}

	pending, err := rig.svc.List(context.Background(), false)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending requests = %v, %v; want exactly one", pending, err)
	}
	req := pending[0]
	if req.Kind != domain.HumanRequestPRMerge || req.ProjectID != "demo" || req.Requester.Kind != domain.HumanRequesterSession || req.Requester.SessionID != rig.worker {
		t.Fatalf("filed request = %+v; it must be a project request attributed to the verified session", req)
	}
	if !strings.Contains(req.Summary, "pull request #7") || !strings.Contains(req.Summary, e2eHead[:12]) {
		t.Fatalf("summary = %q", req.Summary)
	}

	// The person approves what they were shown: the provider is asked to merge exactly that pull request and head.
	done, err := rig.svc.Approve(context.Background(), req.ID, req.PayloadHash)
	if err != nil || done.State != domain.HumanRequestApplied {
		t.Fatalf("approve = %+v, %v", done, err)
	}
	want := prsvc.MergeRequest{PRID: "7", PRURL: "https://github.com/acme/repo/pull/7", ExpectedHeadSHA: e2eHead}
	if got := rig.merger.calls(); len(got) != 1 || got[0] != want {
		t.Fatalf("merge calls = %+v, want exactly %+v", got, want)
	}
}

func TestE2E_AnAgentMergeOnAProjectThatAllowsItGoesStraightToTheProviderWithTheExactHead(t *testing.T) {
	rig := newMergeRig(t, true)
	rig.asSession(t)

	out, errOut, err := executeCLI(t, aliveDeps(), "pr", "merge", "7")
	if err != nil {
		t.Fatalf("ao pr merge 7: %v\nstderr=%s", err, errOut)
	}
	if !strings.Contains(out, "merged PR #7") {
		t.Fatalf("stdout = %q", out)
	}
	want := prsvc.MergeRequest{PRID: "7", PRURL: "https://github.com/acme/repo/pull/7", ExpectedHeadSHA: e2eHead}
	if got := rig.merger.calls(); len(got) != 1 || got[0] != want {
		t.Fatalf("merge calls = %+v, want exactly %+v", got, want)
	}
	if pending, _ := rig.svc.List(context.Background(), false); len(pending) != 0 {
		t.Fatalf("a permitted merge filed %d requests", len(pending))
	}
}

func TestE2E_AnotherAgentCannotMergeThePRAndATerminalCannotMergeItEither(t *testing.T) {
	rig := newMergeRig(t, true) // even with the setting on
	ctx := context.Background()
	other, err := rig.store.CreateSession(ctx, domain.SessionRecord{
		ProjectID: "demo", Kind: domain.KindWorker, Harness: domain.HarnessCodex,
		Activity: domain.Activity{State: domain.ActivityActive, LastActivityAt: time.Now()}, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	otherProof, err := identity.NewIssuer(rig.store, nil).Issue(ctx, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("AO_SESSION_ID", string(other.ID))
	t.Setenv(identity.EnvProof, otherProof)
	if _, _, err := executeCLI(t, aliveDeps(), "pr", "merge", "7"); err == nil || !strings.Contains(err.Error(), "MERGE_NOT_PERMITTED") {
		t.Fatalf("another worker's merge: err = %v; want MERGE_NOT_PERMITTED", err)
	}

	rig.asTerminal(t)
	_, _, _ = executeCLI(t, aliveDeps(), "pr", "merge", "7") // refused, or filed as an unauthenticated terminal: never merged
	if got := rig.merger.calls(); len(got) != 0 {
		t.Fatalf("a caller who is not the PR's owner or manager merged: %+v", got)
	}
}
