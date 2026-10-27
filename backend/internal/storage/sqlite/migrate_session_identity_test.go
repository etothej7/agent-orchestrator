package sqlite

import (
	"strings"
	"testing"
)

const sessionIdentityMigration = int64(20261027020000000)

func TestSessionIdentityMigrationKeepsOneHashPerSessionAndNeverTwoSessionsOnOneHash(t *testing.T) {
	db := openMigratedDatabaseCopy(t, 20261027010000000)
	upTo(t, db, sessionIdentityMigration)

	exec := func(query string, args ...any) error { _, err := db.Exec(query, args...); return err }
	if err := exec(`INSERT INTO projects (id, path, display_name, registered_at) VALUES ('p', '/tmp/p', 'p', CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	for num, id := range []string{"s-1", "s-2"} {
		if err := exec(`INSERT INTO sessions (id, project_id, num, kind, activity_state, activity_last_at, is_terminated, created_at, updated_at)
			VALUES (?, 'p', ?, 'worker', 'idle', CURRENT_TIMESTAMP, 0, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`, id, num+1); err != nil {
			t.Fatal(err)
		}
	}
	hash := func(c string) string { return strings.Repeat(c, 64) }
	insert := func(session, h string) error {
		return exec(`INSERT INTO session_identity (session_id, proof_sha256, issued_at) VALUES (?, ?, CURRENT_TIMESTAMP)`, session, h)
	}
	if err := insert("s-1", hash("a")); err != nil {
		t.Fatalf("first proof: %v", err)
	}
	if err := insert("s-2", hash("a")); err == nil {
		t.Fatal("two sessions were given the same proof hash")
	}
	if err := insert("s-1", hash("b")); err == nil {
		t.Fatal("a second row for one session was accepted (a relaunch must replace the proof, not add one)")
	}
	if err := insert("s-2", "too-short"); err == nil {
		t.Fatal("a hash that is not 64 characters was accepted")
	}
	if err := exec(`INSERT INTO session_identity (session_id, proof_sha256, issued_at, generation) VALUES ('s-2', ?, CURRENT_TIMESTAMP, 0)`, hash("c")); err == nil {
		t.Fatal("generation 0 was accepted")
	}
	// A proof goes with its session.
	var id, seq int
	var table, from, to, onUpdate, onDelete, match string
	rows, err := db.Query(`PRAGMA foreign_key_list(session_identity)`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		t.Fatal("session_identity has no foreign key to sessions")
	}
	if err := rows.Scan(&id, &seq, &table, &from, &to, &onUpdate, &onDelete, &match); err != nil {
		t.Fatal(err)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if table != "sessions" || from != "session_id" || onDelete != "CASCADE" {
		t.Fatalf("foreign key = %s.%s -> %s ON DELETE %s; want sessions.session_id ON DELETE CASCADE", table, from, to, onDelete)
	}
}
