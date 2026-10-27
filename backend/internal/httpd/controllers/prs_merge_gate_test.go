package controllers_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	"github.com/aoagents/agent-orchestrator/backend/internal/identity"
	humanrequestsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/humanrequest"
	prsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/pr"
)

const (
	gatePRURL  = "https://github.com/acme/repo/pull/7"
	gateHead   = "0123456789abcdef0123456789abcdef01234567"
	gateBody   = `{"prUrl":"` + gatePRURL + `","expectedHeadSha":"` + gateHead + `"}`
	gateWorker = domain.SessionID("repo-1")
	gateMgr    = domain.SessionID("repo-orchestrator-1")
)

type gateMerger struct{ merged []prsvc.MergeRequest }

func (g *gateMerger) Merge(_ context.Context, r prsvc.MergeRequest) (prsvc.MergeResult, error) {
	g.merged = append(g.merged, r)
	return prsvc.MergeResult{PRNumber: 7, Method: "squash", State: domain.PRStateMerged}, nil
}

func (g *gateMerger) ResolveComments(context.Context, string, []string) (prsvc.ResolveResult, error) {
	return prsvc.ResolveResult{}, nil
}

type gateStore struct {
	agentMayMerge bool
}

func (g gateStore) GetPR(_ context.Context, url string) (domain.PullRequest, bool, error) {
	if url != gatePRURL {
		return domain.PullRequest{}, false, nil
	}
	return domain.PullRequest{URL: url, SessionID: gateWorker, Number: 7, HeadSHA: gateHead}, true, nil
}

func (g gateStore) GetPRByNumber(_ context.Context, number int) (domain.PullRequest, bool, error) {
	if number != 7 {
		return domain.PullRequest{}, false, nil
	}
	return domain.PullRequest{URL: gatePRURL, SessionID: gateWorker, Number: 7, HeadSHA: gateHead, Title: "Fix"}, true, nil
}

func (g gateStore) GetSession(_ context.Context, id domain.SessionID) (domain.SessionRecord, bool, error) {
	switch id {
	case gateWorker, gateMgr, "repo-2", "other-1":
		project := domain.ProjectID("repo")
		if id == "other-1" {
			project = "other"
		}
		return domain.SessionRecord{ID: id, ProjectID: project}, true, nil
	}
	return domain.SessionRecord{}, false, nil
}

func (g gateStore) GetProject(_ context.Context, id string) (domain.ProjectRecord, bool, error) {
	return domain.ProjectRecord{ID: id, Config: domain.ProjectConfig{AgentMayMerge: g.agentMayMerge && id == "repo"}}, true, nil
}

type gateRequests struct{ filed []humanrequestsvc.FileInput }

func (g *gateRequests) File(_ context.Context, in humanrequestsvc.FileInput) (domain.HumanRequest, error) {
	g.filed = append(g.filed, in)
	return domain.HumanRequest{ID: "hr-9", Kind: in.Kind, ProjectID: in.ProjectID, Requester: in.Requester, State: domain.HumanRequestPending}, nil
}
func (g *gateRequests) List(context.Context, bool) ([]domain.HumanRequest, error) { return nil, nil }
func (g *gateRequests) Get(context.Context, string) (domain.HumanRequest, error) {
	return domain.HumanRequest{}, nil
}
func (g *gateRequests) Approve(context.Context, string, string) (domain.HumanRequest, error) {
	return domain.HumanRequest{}, nil
}
func (g *gateRequests) Deny(context.Context, string) (domain.HumanRequest, error) {
	return domain.HumanRequest{}, nil
}

// gateReproof is -6's re-proof service in miniature: it acts only for a live session that has no proof.
type gateReproof struct {
	asked []domain.SessionID
	live  map[domain.SessionID]bool
}

func (g *gateReproof) Prioritize(_ context.Context, id domain.SessionID) bool {
	if !g.live[id] {
		return false
	}
	g.asked = append(g.asked, id)
	return true
}

type gateRig struct {
	merger   *gateMerger
	requests *gateRequests
	reproof  *gateReproof
	claim    domain.SessionID
	caller   identity.Caller
	router   chi.Router
}

func newGateRig(store gateStore) *gateRig {
	g := &gateRig{merger: &gateMerger{}, requests: &gateRequests{}, reproof: &gateReproof{live: map[domain.SessionID]bool{gateWorker: true}}}
	g.router = chi.NewRouter()
	g.router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(identity.WithCaller(r.Context(), g.caller)))
		})
	})
	(&controllers.PRsController{Svc: g.merger, Store: store, Requests: g.requests, Reproof: g.reproof}).Register(g.router)
	return g
}

func (g *gateRig) merge(caller identity.Caller) (int, string) {
	g.caller = caller
	req := httptest.NewRequest(http.MethodPost, "/prs/7/merge", strings.NewReader(gateBody))
	if g.claim != "" {
		req.Header.Set(identity.HeaderClaim, string(g.claim))
	}
	rec := httptest.NewRecorder()
	g.router.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func agentCaller(id domain.SessionID, kind domain.SessionKind) identity.Caller {
	return identity.Caller{Kind: identity.KindAgent, Session: id, SessionKind: kind, Project: "repo"}
}

func TestThePersonAlwaysMergesWhateverTheProjectSays(t *testing.T) {
	g := newGateRig(gateStore{agentMayMerge: false})
	if status, body := g.merge(identity.Human()); status != http.StatusOK || len(g.merger.merged) != 1 {
		t.Fatalf("the person's merge = %d %s, merges %d", status, body, len(g.merger.merged))
	}
}

func TestAnAgentMergesItsOwnPRAndAManagerMergesTheProjectsOnlyWhenTheProjectAllowsIt(t *testing.T) {
	for name, caller := range map[string]identity.Caller{
		"the PR's own worker":   agentCaller(gateWorker, domain.KindWorker),
		"the project's manager": agentCaller(gateMgr, domain.KindOrchestrator),
	} {
		allowed := newGateRig(gateStore{agentMayMerge: true})
		if status, body := allowed.merge(caller); status != http.StatusOK || len(allowed.merger.merged) != 1 || len(allowed.requests.filed) != 0 {
			t.Errorf("%s, setting on: %d %s (merges %d, requests %d)", name, status, body, len(allowed.merger.merged), len(allowed.requests.filed))
		}
		off := newGateRig(gateStore{agentMayMerge: false})
		status, body := off.merge(caller)
		if status != http.StatusAccepted || len(off.merger.merged) != 0 || len(off.requests.filed) != 1 {
			t.Fatalf("%s, setting off: %d %s (merges %d, requests %d)", name, status, body, len(off.merger.merged), len(off.requests.filed))
		}
		filed := off.requests.filed[0]
		if filed.Kind != domain.HumanRequestPRMerge || filed.ProjectID != "repo" || filed.SessionID != "" ||
			filed.Requester.Kind != domain.HumanRequesterSession || filed.Requester.SessionID != caller.Session {
			t.Errorf("%s: filed %+v", name, filed)
		}
		if !strings.Contains(body, `"id":"hr-9"`) || !strings.Contains(body, "Do not retry") {
			t.Errorf("%s: body %s", name, body)
		}
	}
}

func TestAnAgentThatIsNeitherTheOwnerNorTheManagerIsRefusedWhateverTheSetting(t *testing.T) {
	for name, caller := range map[string]identity.Caller{
		"another worker":            agentCaller("repo-2", domain.KindWorker),
		"another project's manager": {Kind: identity.KindAgent, Session: "other-1", SessionKind: domain.KindOrchestrator, Project: "other"},
		"a reviewer":                {Kind: identity.KindReviewer, Review: "rev-1", Worker: gateWorker},
	} {
		g := newGateRig(gateStore{agentMayMerge: true})
		status, body := g.merge(caller)
		if status != http.StatusForbidden || !strings.Contains(body, "MERGE_NOT_PERMITTED") || len(g.merger.merged) != 0 || len(g.requests.filed) != 0 {
			t.Errorf("%s: %d %s (merges %d, requests %d)", name, status, body, len(g.merger.merged), len(g.requests.filed))
		}
	}
}

func TestACallerWhoProvedNothingFilesARequestAsAnUnauthenticatedTerminal(t *testing.T) {
	g := newGateRig(gateStore{agentMayMerge: true})
	status, body := g.merge(identity.Anonymous())
	if status != http.StatusAccepted || len(g.merger.merged) != 0 || len(g.requests.filed) != 1 {
		t.Fatalf("anonymous merge = %d %s (merges %d, requests %d)", status, body, len(g.merger.merged), len(g.requests.filed))
	}
	if r := g.requests.filed[0].Requester; r.Kind != domain.HumanRequesterTerminal || r.SessionID != "" {
		t.Fatalf("requester = %+v", r)
	}
	if len(g.reproof.asked) != 0 {
		t.Fatal("a terminal was asked to be re-proved")
	}
}

func TestAnUnprovenSessionIsToldToBeRelaunchedAndGetsNoRequest(t *testing.T) {
	g := newGateRig(gateStore{agentMayMerge: false})
	g.claim = gateWorker
	status, body := g.merge(identity.Anonymous())
	if status != http.StatusForbidden || !strings.Contains(body, "SESSION_NEEDS_IDENTITY") || !strings.Contains(body, "relaunched automatically when idle") {
		t.Fatalf("= %d %s", status, body)
	}
	if len(g.requests.filed) != 0 || len(g.merger.merged) != 0 {
		t.Fatal("an unproven session got a request or a merge")
	}
	if len(g.reproof.asked) != 1 || g.reproof.asked[0] != gateWorker {
		t.Fatalf("re-proof asked for %v", g.reproof.asked)
	}
}

func TestAClaimToBeAnEndedOrUnknownSessionIsJustATerminal(t *testing.T) {
	for name, claim := range map[string]domain.SessionID{
		"an ended session":   "repo-2", // not live: the re-proof service does not act for it
		"an unknown session": "ghost-1",
	} {
		g := newGateRig(gateStore{})
		g.claim = claim
		status, _ := g.merge(identity.Anonymous())
		if status != http.StatusAccepted || len(g.requests.filed) != 1 || len(g.reproof.asked) != 0 {
			t.Errorf("%s: %d, requests %d, reproof %v", name, status, len(g.requests.filed), g.reproof.asked)
		}
	}
}

func TestAPRTheDaemonDoesNotTrackIsNotFoundToEveryoneButThePerson(t *testing.T) {
	g := newGateRig(gateStore{})
	g.caller = identity.Anonymous()
	req := httptest.NewRequest(http.MethodPost, "/prs/7/merge", strings.NewReader(`{"prUrl":"https://github.com/acme/repo/pull/99","expectedHeadSha":"`+gateHead+`"}`))
	rec := httptest.NewRecorder()
	g.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound || len(g.requests.filed) != 0 || len(g.merger.merged) != 0 {
		t.Fatalf("an untracked PR: %d, requests %d merges %d", rec.Code, len(g.requests.filed), len(g.merger.merged))
	}
}
