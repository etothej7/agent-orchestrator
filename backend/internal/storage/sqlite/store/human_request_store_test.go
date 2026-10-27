package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

var humanRequestEpoch = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

func humanRequest(id string, session domain.SessionID, hash string) domain.HumanRequest {
	return domain.HumanRequest{
		ID: id, Kind: domain.HumanRequestAutoResumeLift, SessionID: session,
		Requester:   domain.HumanRequestRequester{Kind: domain.HumanRequesterSession, SessionID: "agent-9"},
		Payload:     json.RawMessage(`{}`),
		PayloadHash: hash, Summary: "Allow automatic resume for Fix the bug.",
		Changes:   []domain.HumanRequestChange{{Field: "autoResume", From: "never", To: "allowed"}},
		CreatedAt: humanRequestEpoch, ExpiresAt: humanRequestEpoch.Add(24 * time.Hour),
	}
}

func TestHumanRequestIsStoredWithItsRenderedFieldsAndFiledOnlyOncePerOpenPayload(t *testing.T) {
	db, session := autoResumeSession(t)
	ctx := context.Background()

	first, inserted, err := db.InsertHumanRequest(ctx, humanRequest("hr-1", session, "hash-a"))
	if err != nil || !inserted {
		t.Fatalf("first filing = %+v, inserted %v, %v", first, inserted, err)
	}
	if first.State != domain.HumanRequestPending || first.Summary == "" || len(first.Changes) != 1 || first.Changes[0].To != "allowed" ||
		first.Requester.SessionID != "agent-9" || string(first.Payload) != `{}` || !first.ExpiresAt.Equal(humanRequestEpoch.Add(24*time.Hour)) {
		t.Fatalf("stored request = %+v", first)
	}

	again, inserted, err := db.InsertHumanRequest(ctx, humanRequest("hr-2", session, "hash-a"))
	if err != nil || inserted || again.ID != "hr-1" {
		t.Fatalf("a retried filing = %+v, inserted %v, %v; want the open hr-1 back", again, inserted, err)
	}
	other, inserted, err := db.InsertHumanRequest(ctx, humanRequest("hr-3", session, "hash-b"))
	if err != nil || !inserted || other.ID != "hr-3" {
		t.Fatalf("another payload = %+v, inserted %v, %v", other, inserted, err)
	}
	all, err := db.ListHumanRequests(ctx, "", 10)
	if err != nil || len(all) != 2 {
		t.Fatalf("listed %d requests, %v; want 2", len(all), err)
	}
}

func TestAClaimedRequestCanBeFiledAgainAndAClaimHappensOnce(t *testing.T) {
	db, session := autoResumeSession(t)
	ctx := context.Background()
	if _, _, err := db.InsertHumanRequest(ctx, humanRequest("hr-1", session, "h")); err != nil {
		t.Fatal(err)
	}
	if ok, err := db.ClaimHumanRequest(ctx, "hr-1", humanRequestEpoch.Add(time.Minute)); err != nil || !ok {
		t.Fatalf("first claim = %v, %v", ok, err)
	}
	if ok, err := db.ClaimHumanRequest(ctx, "hr-1", humanRequestEpoch.Add(2*time.Minute)); err != nil || ok {
		t.Fatalf("second claim = %v, %v; want false", ok, err)
	}
	if ok, _ := db.DenyHumanRequest(ctx, "hr-1", humanRequestEpoch); ok {
		t.Fatal("a claimed request was denied")
	}
	if err := db.FinishHumanRequest(ctx, "hr-1", domain.HumanRequestApplied, "done", humanRequestEpoch.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	got, found, err := db.GetHumanRequest(ctx, "hr-1")
	if err != nil || !found || got.State != domain.HumanRequestApplied || got.Result != "done" || got.DecidedAt.IsZero() {
		t.Fatalf("finished request = %+v, %v, %v", got, found, err)
	}
	// Once it is no longer open, the same payload may be asked for again.
	if again, inserted, err := db.InsertHumanRequest(ctx, humanRequest("hr-2", session, "h")); err != nil || !inserted || again.ID != "hr-2" {
		t.Fatalf("a new filing after the first was applied = %+v, %v, %v", again, inserted, err)
	}
	if err := db.FinishHumanRequest(ctx, "hr-1", domain.HumanRequestApplied, "again", humanRequestEpoch); err == nil {
		t.Fatal("a finished request finished twice")
	}
	if err := db.FinishHumanRequest(ctx, "hr-2", domain.HumanRequestPending, "", humanRequestEpoch); err == nil {
		t.Fatal("finishing into pending was accepted")
	}
}

func TestAnExpiredRequestCannotBeClaimedAndDenyOnlyWorksWhilePending(t *testing.T) {
	db, session := autoResumeSession(t)
	ctx := context.Background()
	if _, _, err := db.InsertHumanRequest(ctx, humanRequest("hr-1", session, "h1")); err != nil {
		t.Fatal(err)
	}
	if ok, err := db.ClaimHumanRequest(ctx, "hr-1", humanRequestEpoch.Add(25*time.Hour)); err != nil || ok {
		t.Fatalf("claim after the deadline = %v, %v; want false", ok, err)
	}
	if ok, err := db.DenyHumanRequest(ctx, "hr-1", humanRequestEpoch.Add(time.Minute)); err != nil || !ok {
		t.Fatalf("deny = %v, %v", ok, err)
	}
	if ok, _ := db.DenyHumanRequest(ctx, "hr-1", humanRequestEpoch.Add(2*time.Minute)); ok {
		t.Fatal("a denied request was denied again")
	}
	if ok, _ := db.ClaimHumanRequest(ctx, "no-such-request", humanRequestEpoch); ok {
		t.Fatal("an unknown request was claimed")
	}
	if _, found, err := db.GetHumanRequest(ctx, "no-such-request"); err != nil || found {
		t.Fatalf("unknown request found = %v, %v", found, err)
	}
}

func TestTheSweepExpiresOverduePendingRequestsAndFailsAbandonedApprovals(t *testing.T) {
	db, session := autoResumeSession(t)
	ctx := context.Background()
	for _, id := range []string{"overdue", "fresh", "abandoned", "recent-claim"} {
		if _, _, err := db.InsertHumanRequest(ctx, humanRequest(id, session, "hash-"+id)); err != nil {
			t.Fatal(err)
		}
	}
	// Extend "fresh" past the sweep time by filing it later with a later deadline.
	late := humanRequest("late", session, "hash-late")
	late.ExpiresAt = humanRequestEpoch.Add(72 * time.Hour)
	if _, _, err := db.InsertHumanRequest(ctx, late); err != nil {
		t.Fatal(err)
	}
	now := humanRequestEpoch.Add(30 * time.Hour)
	if ok, _ := db.ClaimHumanRequest(ctx, "abandoned", humanRequestEpoch.Add(time.Hour)); !ok {
		t.Fatal("claim abandoned")
	}
	if ok, _ := db.ClaimHumanRequest(ctx, "recent-claim", humanRequestEpoch.Add(10*time.Minute)); !ok {
		t.Fatal("claim recent-claim")
	}
	changed, err := db.ExpireHumanRequests(ctx, now, now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]domain.HumanRequestState{}
	for _, req := range changed {
		got[req.ID] = req.State
		if req.Result == "" || req.DecidedAt.IsZero() {
			t.Errorf("%s was changed without a result: %+v", req.ID, req)
		}
	}
	// Every request but "late" is past its 24 h deadline (or an old claim); "late" lives until +72 h.
	if got["overdue"] != domain.HumanRequestExpired || got["fresh"] != domain.HumanRequestExpired ||
		got["abandoned"] != domain.HumanRequestFailed || got["recent-claim"] != domain.HumanRequestFailed {
		t.Fatalf("sweep changed %v", got)
	}
	if _, touched := got["late"]; touched {
		t.Fatal("a request inside its deadline was expired")
	}
	if again, err := db.ExpireHumanRequests(ctx, now, now.Add(-time.Minute)); err != nil || len(again) != 0 {
		t.Fatalf("a second sweep changed %d rows, %v", len(again), err)
	}
}

func TestHumanRequestCountsAndListingFollowRequesterAndState(t *testing.T) {
	db, session := autoResumeSession(t)
	ctx := context.Background()
	agent := domain.HumanRequestRequester{Kind: domain.HumanRequesterSession, SessionID: "agent-9"}
	terminal := domain.HumanRequestRequester{Kind: domain.HumanRequesterTerminal}
	for i, hash := range []string{"a", "b", "c"} {
		req := humanRequest("hr-"+hash, session, hash)
		req.CreatedAt = humanRequestEpoch.Add(time.Duration(i) * time.Minute)
		if i == 2 {
			req.Requester = terminal
		}
		if _, _, err := db.InsertHumanRequest(ctx, req); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := db.CountPendingHumanRequests(ctx, agent); err != nil || n != 2 {
		t.Fatalf("pending for the agent = %d, %v; want 2", n, err)
	}
	if n, err := db.CountPendingHumanRequests(ctx, terminal); err != nil || n != 1 {
		t.Fatalf("pending for the terminal = %d, %v; want 1", n, err)
	}
	if n, err := db.CountHumanRequestsFiledSince(ctx, agent, humanRequestEpoch.Add(30*time.Second)); err != nil || n != 1 {
		t.Fatalf("filed since = %d, %v; want 1", n, err)
	}
	if ok, _ := db.DenyHumanRequest(ctx, "hr-a", humanRequestEpoch.Add(time.Hour)); !ok {
		t.Fatal("deny")
	}
	pending, err := db.ListHumanRequests(ctx, domain.HumanRequestPending, 10)
	if err != nil || len(pending) != 2 || pending[0].ID != "hr-c" || pending[1].ID != "hr-b" {
		t.Fatalf("pending newest first = %+v, %v", pending, err)
	}
	if limited, _ := db.ListHumanRequests(ctx, "", 1); len(limited) != 1 || limited[0].ID != "hr-c" {
		t.Fatalf("limit = %+v", limited)
	}
}

func projectHumanRequest(id, project, hash string) domain.HumanRequest {
	req := humanRequest(id, "", hash)
	req.ProjectID = project
	req.Kind = "project_probe"
	return req
}

func TestAProjectRequestIsStoredFiledOnceAndKeptApartFromASessionRequest(t *testing.T) {
	db, session := autoResumeSession(t)
	ctx := context.Background()

	first, inserted, err := db.InsertHumanRequest(ctx, projectHumanRequest("hr-p1", "repo", "h"))
	if err != nil || !inserted {
		t.Fatalf("first filing = %+v, inserted %v, %v", first, inserted, err)
	}
	if first.ProjectID != "repo" || first.SessionID != "" || first.TargetKind() != domain.HumanTargetProject || first.TargetKey() != "project:repo" {
		t.Fatalf("stored target = session %q project %q (%s)", first.SessionID, first.ProjectID, first.TargetKind())
	}
	again, inserted, err := db.InsertHumanRequest(ctx, projectHumanRequest("hr-p2", "repo", "h"))
	if err != nil || inserted || again.ID != "hr-p1" {
		t.Fatalf("a retried filing = %+v, inserted %v, %v; want the open hr-p1 back", again, inserted, err)
	}
	// The same kind and hash aimed at a session is a different request.
	sessionReq := humanRequest("hr-s1", session, "h")
	sessionReq.Kind = "project_probe"
	if _, inserted, err := db.InsertHumanRequest(ctx, sessionReq); err != nil || !inserted {
		t.Fatalf("a session request with the same kind and hash = inserted %v, %v", inserted, err)
	}
	found, ok, err := db.FindOpenHumanRequest(ctx, "project_probe", "project:repo", "h")
	if err != nil || !ok || found.ID != "hr-p1" {
		t.Fatalf("find by project key = %+v, %v, %v", found, ok, err)
	}
	found, ok, err = db.FindOpenHumanRequest(ctx, "project_probe", string(session), "h")
	if err != nil || !ok || found.ID != "hr-s1" {
		t.Fatalf("find by session key = %+v, %v, %v", found, ok, err)
	}
}

func TestARequestNeedsExactlyOneTarget(t *testing.T) {
	db, session := autoResumeSession(t)
	both := humanRequest("hr-both", session, "h")
	both.ProjectID = "repo"
	if _, _, err := db.InsertHumanRequest(context.Background(), both); err == nil {
		t.Fatal("a request about a session and a project was stored")
	}
	if _, _, err := db.InsertHumanRequest(context.Background(), humanRequest("hr-none", "", "h")); err == nil {
		t.Fatal("a request about nothing was stored")
	}
}
