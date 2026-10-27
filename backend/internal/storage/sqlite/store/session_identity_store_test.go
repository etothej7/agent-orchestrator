package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/identity"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

func TestSessionProofHashIsStoredFoundAndReplacedByARelaunch(t *testing.T) {
	db, session := autoResumeSession(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	first, second := strings.Repeat("a", 64), strings.Repeat("b", 64)

	if _, _, found, err := db.SessionIdentityByProofHash(ctx, first); err != nil || found {
		t.Fatalf("lookup before any proof = found %v, %v", found, err)
	}
	if gen, err := db.SessionProofGeneration(ctx, session); err != nil || gen != 0 {
		t.Fatalf("generation before any proof = %d, %v", gen, err)
	}
	if err := db.SetSessionProofHash(ctx, session, first, now); err != nil {
		t.Fatal(err)
	}
	id, stored, found, err := db.SessionIdentityByProofHash(ctx, first)
	if err != nil || !found || id != session || stored != first {
		t.Fatalf("lookup = %q %q found %v, %v; want the session and its hash", id, stored, found, err)
	}

	// A relaunch replaces the proof: the old one stops resolving, the new one resolves.
	if err := db.SetSessionProofHash(ctx, session, second, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, _, found, _ := db.SessionIdentityByProofHash(ctx, first); found {
		t.Fatal("the previous proof still resolves after a relaunch")
	}
	if id, _, found, _ := db.SessionIdentityByProofHash(ctx, second); !found || id != session {
		t.Fatalf("the new proof does not resolve to the session (found %v, id %q)", found, id)
	}
	if gen, _ := db.SessionProofGeneration(ctx, session); gen != 2 {
		t.Fatalf("generation after one relaunch = %d, want 2", gen)
	}

	if err := db.DeleteSessionProof(ctx, session); err != nil {
		t.Fatal(err)
	}
	if _, _, found, _ := db.SessionIdentityByProofHash(ctx, second); found {
		t.Fatal("a revoked proof still resolves")
	}
}

func TestReviewProofHashIsStoredFoundReplacedAndRevoked(t *testing.T) {
	db, session := autoResumeSession(t)
	ctx := context.Background()
	review := seedReviewRow(t, db, session)
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	first, second := strings.Repeat("a", 64), strings.Repeat("b", 64)

	if _, _, found, err := db.ReviewIdentityByProofHash(ctx, first); err != nil || found {
		t.Fatalf("lookup before any proof = found %v, %v", found, err)
	}
	if err := db.SetReviewProofHash(ctx, review, first, now); err != nil {
		t.Fatal(err)
	}
	id, stored, found, err := db.ReviewIdentityByProofHash(ctx, first)
	if err != nil || !found || id != review || stored != first {
		t.Fatalf("lookup = %q %q found %v, %v; want the review and its hash", id, stored, found, err)
	}
	if err := db.SetReviewProofHash(ctx, review, second, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, _, found, _ := db.ReviewIdentityByProofHash(ctx, first); found {
		t.Fatal("the previous reviewer proof still resolves after a relaunch")
	}
	if gen, _ := db.ReviewProofGeneration(ctx, review); gen != 2 {
		t.Fatalf("generation after one relaunch = %d, want 2", gen)
	}
	// A reviewer proof never resolves as a worker/orchestrator session proof, and vice versa.
	if _, _, found, _ := db.SessionIdentityByProofHash(ctx, second); found {
		t.Fatal("a reviewer's proof resolved as a session proof")
	}
	if err := db.DeleteReviewProof(ctx, review); err != nil {
		t.Fatal(err)
	}
	if _, _, found, _ := db.ReviewIdentityByProofHash(ctx, second); found {
		t.Fatal("a revoked reviewer proof still resolves")
	}
}

func seedReviewRow(t *testing.T, db *sqlite.Store, session domain.SessionID) string {
	t.Helper()
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	if err := db.UpsertReview(context.Background(), domain.Review{
		ID: "review-1", SessionID: session, ProjectID: "repo", Harness: domain.ReviewerHarness("codex"),
		PRURL: "https://example.test/pr/1", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	return "review-1"
}

// The real store, issuer and resolver together: a reviewer proof resolves to its review and
// worker, and ends with the worker.
func TestReviewerProofResolvesThroughTheRealStore(t *testing.T) {
	db, session := autoResumeSession(t)
	ctx := context.Background()
	review := seedReviewRow(t, db, session)
	issuer := identity.NewIssuer(db, nil).WithReviews(db)
	resolver := identity.NewResolver(db, db).WithReviews(db, db)

	proof, err := issuer.IssueReview(ctx, review)
	if err != nil {
		t.Fatal(err)
	}
	caller, err := resolver.Resolve(ctx, proof)
	if err != nil || !caller.IsReviewer() || caller.Review != review || caller.Worker != session {
		t.Fatalf("resolved %+v, %v; want the reviewer of %s reviewing %s", caller, err, review, session)
	}
	if caller.IsAgent() {
		t.Fatal("a reviewer resolved as an agent session")
	}
}

func TestSessionsWithProofIsOneBatchedAnswerForTheSessionList(t *testing.T) {
	db, session := autoResumeSession(t)
	ctx := context.Background()
	if got, err := db.SessionsWithProof(ctx, nil); err != nil || len(got) != 0 {
		t.Fatalf("no ids = %v, %v", got, err)
	}
	if got, err := db.SessionsWithProof(ctx, []domain.SessionID{session, "never-issued"}); err != nil || len(got) != 0 {
		t.Fatalf("before any proof = %v, %v", got, err)
	}
	if err := db.SetSessionProofHash(ctx, session, strings.Repeat("a", 64), time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err := db.SessionsWithProof(ctx, []domain.SessionID{session, "never-issued"})
	if err != nil || len(got) != 1 || !got[session] {
		t.Fatalf("after the proof = %v, %v; want only the proven session", got, err)
	}
}
