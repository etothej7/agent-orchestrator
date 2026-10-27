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

// ED8 P2, two-phase rotation: while a proof is in use, a new one only waits beside it; the old one
// keeps working until the new one is first presented (the new process is up), and a launch that
// never happens leaves the old proof working.
func TestAProofInUseIsOnlyReplacedWhenItsSuccessorIsFirstUsed(t *testing.T) {
	db, session := autoResumeSession(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	old, next, third := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	resolves := func(hash string) bool {
		_, _, found, err := db.SessionIdentityByProofHash(ctx, hash)
		if err != nil {
			t.Fatal(err)
		}
		return found
	}

	// A session's FIRST proof is active at once but unconfirmed.
	if err := db.SetSessionProofHash(ctx, session, old, now); err != nil {
		t.Fatal(err)
	}
	if ok, _ := db.SessionProofConfirmed(ctx, session); ok {
		t.Fatal("a proof nobody presented is confirmed")
	}
	// Until it is used nothing can depend on it, so a new one replaces it outright.
	if err := db.SetSessionProofHash(ctx, session, next, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if resolves(old) || !resolves(next) {
		t.Fatalf("an unused proof was not replaced (old %v, next %v)", resolves(old), resolves(next))
	}
	// The proof is used: confirmed.
	if err := db.ConfirmSessionProof(ctx, session, next, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if ok, _ := db.SessionProofConfirmed(ctx, session); !ok {
		t.Fatal("a presented proof is not confirmed")
	}

	// A relaunch issues a successor: both verify, the old one keeps working.
	if err := db.SetSessionProofHash(ctx, session, third, now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if !resolves(next) || !resolves(third) {
		t.Fatalf("with a pending proof: active %v, pending %v; want both to verify", resolves(next), resolves(third))
	}
	// The launch FAILED: the pending proof is never presented. The old one still works.
	if gen, _ := db.SessionProofGeneration(ctx, session); gen != 2 {
		t.Fatalf("generation = %d, want 2 (the pending proof is not a new generation until used)", gen)
	}
	// The new process calls: the pending proof is promoted and the old one dies.
	if err := db.ConfirmSessionProof(ctx, session, third, now.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if resolves(next) || !resolves(third) {
		t.Fatalf("after promotion: old %v, new %v; want only the new one", resolves(next), resolves(third))
	}
	if gen, _ := db.SessionProofGeneration(ctx, session); gen != 3 {
		t.Fatalf("generation after promotion = %d, want 3", gen)
	}
	// Presenting the (now active) proof again changes nothing.
	if err := db.ConfirmSessionProof(ctx, session, third, now.Add(5*time.Minute)); err != nil || !resolves(third) {
		t.Fatalf("re-confirming the active proof: %v, resolves %v", err, resolves(third))
	}
}

func TestAnotherPendingProofReplacesThePreviousPendingOne(t *testing.T) {
	db, session := autoResumeSession(t)
	ctx := context.Background()
	now := time.Now()
	h := func(c string) string { return strings.Repeat(c, 64) }
	_ = db.SetSessionProofHash(ctx, session, h("a"), now)
	_ = db.ConfirmSessionProof(ctx, session, h("a"), now)
	_ = db.SetSessionProofHash(ctx, session, h("b"), now) // pending (a failed launch)
	_ = db.SetSessionProofHash(ctx, session, h("c"), now) // pending again (the retry)
	resolves := func(x string) bool { _, _, f, _ := db.SessionIdentityByProofHash(ctx, h(x)); return f }
	if !resolves("a") || resolves("b") || !resolves("c") {
		t.Fatalf("a=%v b=%v c=%v; want the active and only the latest pending", resolves("a"), resolves("b"), resolves("c"))
	}
}

// "This session once had a proof presented" is durable: revoking the proof removes what verifies
// but keeps the fact, so it cannot reopen the process-ancestry bridge or the coordinator's first
// phase; and the session no longer counts as holding a proof.
func TestRevokingAProofKeepsTheEverProvenMarker(t *testing.T) {
	db, session := autoResumeSession(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	hash := strings.Repeat("c", 64)
	if err := db.SetSessionProofHash(ctx, session, hash, now); err != nil {
		t.Fatal(err)
	}
	if err := db.ConfirmSessionProof(ctx, session, hash, now); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteSessionProof(ctx, session); err != nil {
		t.Fatal(err)
	}
	if confirmed, err := db.SessionProofConfirmed(ctx, session); err != nil || !confirmed {
		t.Fatalf("after a revoke the session is not marked as ever proven (%v, %v)", confirmed, err)
	}
	if gen, _ := db.SessionProofGeneration(ctx, session); gen != 1 {
		t.Fatalf("generation after a revoke = %d, want 1 (kept)", gen)
	}
	if _, _, found, _ := db.SessionIdentityByProofHash(ctx, hash); found {
		t.Fatal("the revoked proof still resolves")
	}
	held, err := db.SessionsWithProof(ctx, []domain.SessionID{session})
	if err != nil || held[session] {
		t.Fatalf("a revoked session still reports a proof (%v, %v)", held, err)
	}
	// a new proof can be issued afterwards and works once presented
	fresh := strings.Repeat("d", 64)
	if err := db.SetSessionProofHash(ctx, session, fresh, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := db.ConfirmSessionProof(ctx, session, fresh, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if id, _, found, _ := db.SessionIdentityByProofHash(ctx, fresh); !found || id != session {
		t.Fatalf("a proof issued after a revoke does not resolve (found %v, %q)", found, id)
	}
	held, _ = db.SessionsWithProof(ctx, []domain.SessionID{session})
	if !held[session] {
		t.Fatal("the re-issued proof is not counted")
	}
}
