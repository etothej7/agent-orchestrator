package sqlite

import (
	"strings"
	"testing"
)

const reviewIdentityMigration = int64(20261027144129198)

func TestReviewIdentityMigrationKeepsOneHashPerReviewerAndFollowsTheReviewRow(t *testing.T) {
	db := openMigratedDatabaseCopy(t, 20261027020000000)
	upTo(t, db, reviewIdentityMigration)

	exec := func(query string, args ...any) error { _, err := db.Exec(query, args...); return err }
	if err := exec(`INSERT INTO projects (id, path, display_name, registered_at) VALUES ('p', '/tmp/p', 'p', CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	if err := exec(`INSERT INTO sessions (id, project_id, num, kind, activity_state, activity_last_at, is_terminated, created_at, updated_at)
		VALUES ('s-1', 'p', 1, 'worker', 'idle', CURRENT_TIMESTAMP, 0, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	for id, harness := range map[string]string{"review-1": "codex", "review-2": "claude-code"} {
		if err := exec(`INSERT INTO review (id, session_id, project_id, harness, pr_url, created_at, updated_at)
			VALUES (?, 's-1', 'p', ?, 'https://example.test/pr/1', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`, id, harness); err != nil {
			t.Fatalf("seed review %s: %v", id, err)
		}
	}
	hash := func(c string) string { return strings.Repeat(c, 64) }
	insert := func(review, h string) error {
		return exec(`INSERT INTO review_identity (review_id, proof_sha256, issued_at) VALUES (?, ?, CURRENT_TIMESTAMP)`, review, h)
	}
	if err := insert("review-1", hash("a")); err != nil {
		t.Fatalf("first proof: %v", err)
	}
	if err := insert("review-2", hash("a")); err == nil {
		t.Fatal("two reviewers were given the same proof hash")
	}
	if err := insert("review-1", hash("b")); err == nil {
		t.Fatal("a second row for one reviewer was accepted (a relaunch must replace the proof)")
	}
	if err := insert("review-2", "too-short"); err == nil {
		t.Fatal("a hash that is not 64 characters was accepted")
	}
	if err := insert("no-such-review", hash("c")); err == nil {
		t.Fatal("a proof for a review that does not exist was accepted")
	}

	// The proof goes with its review row.
	if err := exec(`DELETE FROM review WHERE id = 'review-1'`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM review_identity`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("proofs left after deleting their review = %d (%v)", n, err)
	}
}
