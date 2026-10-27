package sqlite

import (
	"slices"
	"testing"
)

const humanRequestsMigration = int64(20261027010000000)

func TestHumanRequestsMigrationCreatesAClosedStateSetAndOneOpenRequestPerPayload(t *testing.T) {
	db := openMigratedDatabaseCopy(t, 20261027000000000)
	upTo(t, db, humanRequestsMigration)

	exec := func(query string, args ...any) error { _, err := db.Exec(query, args...); return err }
	if err := exec(`INSERT INTO projects (id, path, display_name, registered_at) VALUES ('p', '/tmp/p', 'p', CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	if err := exec(`INSERT INTO sessions (id, project_id, num, kind, activity_state, activity_last_at, is_terminated, created_at, updated_at)
		VALUES ('s-1', 'p', 1, 'worker', 'idle', CURRENT_TIMESTAMP, 0, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	insert := func(id, hash, state string) error {
		return exec(`INSERT INTO human_requests (id, kind, session_id, requester_kind, payload, payload_hash, summary, changes, state, created_at, expires_at)
			VALUES (?, 'auto_resume_lift', 's-1', 'terminal', '{}', ?, 's', '[]', ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`, id, hash, state)
	}
	if err := insert("a", "h", "pending"); err != nil {
		t.Fatalf("first pending request: %v", err)
	}
	if err := insert("b", "h", "pending"); err == nil {
		t.Fatal("a second open request for the same kind, session and payload was accepted")
	}
	if err := insert("c", "h", "applied"); err != nil {
		t.Fatalf("a decided request with the same payload must be allowed: %v", err)
	}
	if err := insert("d", "h2", "maybe"); err == nil {
		t.Fatal("an unknown state was accepted")
	}
	if err := exec(`INSERT INTO human_requests (id, kind, session_id, requester_kind, payload, payload_hash, summary, changes, state, created_at, expires_at)
		VALUES ('e', 'x', 's-1', 'agent', '{}', 'h3', 's', '[]', 'pending', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`); err == nil {
		t.Fatal("an unknown requester kind was accepted")
	}
	if err := exec(`INSERT INTO human_requests (id, kind, session_id, requester_kind, payload, payload_hash, summary, changes, state, created_at, expires_at)
		VALUES ('f', 'x', 's-1', 'terminal', 'not json', 'h4', 's', '[]', 'pending', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`); err == nil {
		t.Fatal("a payload that is not JSON was accepted")
	}
	// A request goes with its session.
	var table, from, to, onDelete string
	var id, seq int
	var onUpdate, match string
	rows, err := db.Query(`PRAGMA foreign_key_list(human_requests)`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		t.Fatal("human_requests has no foreign key to sessions")
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

const humanRequestTargetsMigration = int64(20261027030000000)

func TestHumanRequestTargetsMigrationKeepsRequestsAndAddsAProjectTarget(t *testing.T) {
	db := openMigratedDatabaseCopy(t, humanRequestsMigration)
	exec := func(query string, args ...any) error { _, err := db.Exec(query, args...); return err }
	if err := exec(`INSERT INTO projects (id, path, display_name, registered_at) VALUES ('p', '/tmp/p', 'p', CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	if err := exec(`INSERT INTO sessions (id, project_id, num, kind, activity_state, activity_last_at, is_terminated, created_at, updated_at)
		VALUES ('s-1', 'p', 1, 'worker', 'idle', CURRENT_TIMESTAMP, 0, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	if err := exec(`INSERT INTO human_requests (id, kind, session_id, requester_kind, payload, payload_hash, summary, changes, state, created_at, expires_at)
		VALUES ('old', 'auto_resume_lift', 's-1', 'terminal', '{}', 'h', 'sum', '[]', 'pending', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	upTo(t, db, humanRequestTargetsMigration)

	var session, project any
	var summary string
	if err := db.QueryRow(`SELECT session_id, project_id, summary FROM human_requests WHERE id = 'old'`).Scan(&session, &project, &summary); err != nil {
		t.Fatalf("a request filed before the migration was lost: %v", err)
	}
	if session != "s-1" || project != nil || summary != "sum" {
		t.Fatalf("migrated request = session %v project %v summary %q", session, project, summary)
	}

	insert := func(id, sessionID, projectID, hash, state string) error {
		var sessionArg, projectArg any
		if sessionID != "" {
			sessionArg = sessionID
		}
		if projectID != "" {
			projectArg = projectID
		}
		return exec(`INSERT INTO human_requests (id, kind, session_id, project_id, requester_kind, payload, payload_hash, summary, changes, state, created_at, expires_at)
			VALUES (?, 'k', ?, ?, 'terminal', '{}', ?, 's', '[]', ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`, id, sessionArg, projectArg, hash, state)
	}
	if err := insert("p1", "", "p", "h", "pending"); err != nil {
		t.Fatalf("a project-targeted request: %v", err)
	}
	if err := insert("p2", "", "p", "h", "pending"); err == nil {
		t.Fatal("a second open request for the same kind, project and payload was accepted")
	}
	if err := insert("p3", "", "p", "h", "denied"); err != nil {
		t.Fatalf("a decided project request with the same payload must be allowed: %v", err)
	}
	if err := insert("s2", "s-1", "", "h", "pending"); err != nil {
		t.Fatalf("a session request with the same kind and hash as a project request must not collide: %v", err)
	}
	if err := insert("both", "s-1", "p", "hb", "pending"); err == nil {
		t.Fatal("a request about a session and a project at once was accepted")
	}
	if err := insert("neither", "", "", "hn", "pending"); err == nil {
		t.Fatal("a request about nothing was accepted")
	}
	if err := insert("ghost", "", "nope", "hg", "pending"); err == nil {
		t.Fatal("a request about a project that does not exist was accepted")
	}
	// A request goes with what it is about: its session or its project.
	rows, err := db.Query(`SELECT f."table", f."from", f."on_delete" FROM pragma_foreign_key_list('human_requests') f ORDER BY f."table"`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var got []string
	for rows.Next() {
		var table, from, onDelete string
		if err := rows.Scan(&table, &from, &onDelete); err != nil {
			t.Fatal(err)
		}
		got = append(got, table+"."+from+" "+onDelete)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if want := []string{"projects.project_id CASCADE", "sessions.session_id CASCADE"}; !slices.Equal(got, want) {
		t.Fatalf("foreign keys = %v, want %v", got, want)
	}
}
