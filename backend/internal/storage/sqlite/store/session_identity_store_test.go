package store_test

import (
	"context"
	"strings"
	"testing"
	"time"
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
