package identity

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

type fakeReviewStore struct {
	hashes map[string]string
	fail   bool
}

func (f *fakeReviewStore) SetReviewProofHash(_ context.Context, id string, hash string, _ time.Time) error {
	f.hashes[id] = hash
	return nil
}

func (f *fakeReviewStore) ReviewIdentityByProofHash(_ context.Context, hash string) (string, string, bool, error) {
	if f.fail {
		return "", "", false, errors.New("disk on fire")
	}
	for id, h := range f.hashes {
		if h == hash {
			return id, h, true, nil
		}
	}
	return "", "", false, nil
}

func (f *fakeReviewStore) DeleteReviewProof(_ context.Context, id string) error {
	delete(f.hashes, id)
	return nil
}

type fakeReviews map[string]domain.Review

func (f fakeReviews) GetReviewByID(_ context.Context, id string) (domain.Review, bool, error) {
	r, ok := f[id]
	return r, ok, nil
}

func reviewerFixture() (*Issuer, *Resolver, *fakeReviewStore, fakeSessions) {
	sessionStore, sessions := newFakeStore(), fakeSessions{
		"worker-1":  {ID: "worker-1", ProjectID: "repo", Kind: domain.KindWorker},
		"worker-2":  {ID: "worker-2", ProjectID: "repo", Kind: domain.KindWorker},
		"manager-1": {ID: "manager-1", ProjectID: "repo", Kind: domain.KindOrchestrator},
	}
	reviewStore := &fakeReviewStore{hashes: map[string]string{}}
	reviews := fakeReviews{
		"review-1": {ID: "review-1", SessionID: "worker-1", ProjectID: "repo"},
		"review-2": {ID: "review-2", SessionID: "worker-2", ProjectID: "repo"},
	}
	issuer := NewIssuer(sessionStore, nil).WithReviews(reviewStore)
	resolver := NewResolver(sessionStore, sessions).WithReviews(reviewStore, reviews)
	return issuer, resolver, reviewStore, sessions
}

// F1: a native reviewer is a `review` row, not a session, and still needs an identity so that
// `ao review submit` can be limited to the reviewer of that run.
func TestAReviewerHoldsAProofAndResolvesToThatReviewAndItsWorker(t *testing.T) {
	issuer, resolver, _, _ := reviewerFixture()
	proof, err := issuer.IssueReview(context.Background(), "review-1")
	if err != nil {
		t.Fatal(err)
	}
	if !WellFormed(proof) {
		t.Fatalf("reviewer proof %q is not well formed", proof)
	}
	caller, err := resolver.Resolve(context.Background(), proof)
	if err != nil {
		t.Fatal(err)
	}
	if caller.Kind != KindReviewer || caller.Review != "review-1" || caller.Worker != "worker-1" || caller.Project != "repo" {
		t.Fatalf("caller = %+v, want the reviewer of review-1 reviewing worker-1", caller)
	}
	if !caller.IsReviewer() {
		t.Fatal("IsReviewer = false")
	}
	// A reviewer is never an agent session: a predicate written for sessions cannot be satisfied
	// by a reviewer's proof, nor a reviewer predicate by a session's.
	if caller.IsAgent() || caller.IsHuman() {
		t.Fatalf("a reviewer was taken for an agent or the human: %+v", caller)
	}
	workerProof, err := issuer.Issue(context.Background(), "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	worker, err := resolver.Resolve(context.Background(), workerProof)
	if err != nil || worker.IsReviewer() || !worker.IsAgent() {
		t.Fatalf("the worker's own proof resolved to %+v, %v; it must not carry reviewer authority", worker, err)
	}
}

func TestEachReviewerResolvesToItsOwnReviewNeverAnother(t *testing.T) {
	issuer, resolver, _, _ := reviewerFixture()
	one, _ := issuer.IssueReview(context.Background(), "review-1")
	two, _ := issuer.IssueReview(context.Background(), "review-2")
	if one == two {
		t.Fatal("two reviewers were given one proof")
	}
	c1, _ := resolver.Resolve(context.Background(), one)
	c2, _ := resolver.Resolve(context.Background(), two)
	if c1.Review != "review-1" || c2.Review != "review-2" || c1.Worker == c2.Worker {
		t.Fatalf("callers = %+v and %+v", c1, c2)
	}
}

func TestAReviewerRelaunchReplacesItsProofAndRevokeEndsIt(t *testing.T) {
	issuer, resolver, _, _ := reviewerFixture()
	first, _ := issuer.IssueReview(context.Background(), "review-1")
	second, _ := issuer.IssueReview(context.Background(), "review-1")
	if first == second {
		t.Fatal("a relaunch reused the reviewer's proof")
	}
	if _, err := resolver.Resolve(context.Background(), first); !errors.Is(err, ErrInvalid) {
		t.Fatalf("the replaced proof = %v, want invalid", err)
	}
	if c, err := resolver.Resolve(context.Background(), second); err != nil || c.Review != "review-1" {
		t.Fatalf("the current proof = %+v, %v", c, err)
	}
	if err := issuer.RevokeReview(context.Background(), "review-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.Resolve(context.Background(), second); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a revoked proof = %v, want invalid", err)
	}
}

// A reviewer's authority ends with the worker it reviews, and with its review row.
func TestAReviewersProofStopsWhenItsWorkerEndsOrItsReviewIsGone(t *testing.T) {
	issuer, resolver, store, sessions := reviewerFixture()
	proof, _ := issuer.IssueReview(context.Background(), "review-1")

	rec := sessions["worker-1"]
	rec.IsTerminated = true
	sessions["worker-1"] = rec
	if _, err := resolver.Resolve(context.Background(), proof); !errors.Is(err, ErrSessionEnded) {
		t.Fatalf("after the worker ended = %v, want ErrSessionEnded", err)
	}

	delete(sessions, "worker-2")
	proof2, _ := issuer.IssueReview(context.Background(), "review-2")
	if _, err := resolver.Resolve(context.Background(), proof2); !errors.Is(err, ErrSessionEnded) {
		t.Fatalf("with the worker gone = %v, want ErrSessionEnded", err)
	}

	// A store failure is an error, never a fall-through to anonymous-with-authority.
	store.fail = true
	if c, err := resolver.Resolve(context.Background(), proof); err == nil || c.Kind != KindAnonymous {
		t.Fatalf("store failure resolved to %+v, %v", c, err)
	}
}

func TestReviewerProofsNeedAReviewStore(t *testing.T) {
	store := newFakeStore()
	if _, err := NewIssuer(store, nil).IssueReview(context.Background(), "review-1"); err == nil {
		t.Fatal("IssueReview succeeded on an issuer with no review store")
	}
	// A resolver with no review store simply does not know reviewer proofs.
	issuer, _, _, _ := reviewerFixture()
	proof, _ := issuer.IssueReview(context.Background(), "review-1")
	if _, err := NewResolver(store, fakeSessions{}).Resolve(context.Background(), proof); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a reviewer proof on a session-only resolver = %v, want invalid", err)
	}
}
