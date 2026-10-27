package sqlite

import (
	"database/sql"
	"testing"
	"time"
)

const autoResumeMigration = int64(20261027000000000)

func TestAutoResumeMigrationProtectsTheTwoNamedOrchestratorsOnly(t *testing.T) {
	db := openMigratedDatabaseCopy(t, 20261026004000000)
	created := time.Date(2026, 10, 22, 21, 0, 0, 0, time.UTC)
	if _, err := db.Exec(`INSERT INTO projects (id, path, display_name, registered_at) VALUES ('optimus_db', '/tmp/optimus_db', 'db', ?)`, created); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	for n, id := range []string{"optimus_db-1", "optimus_rails_app-1", "optimus_db-2", "agent-1"} {
		if _, err := db.Exec(`INSERT INTO sessions (id, project_id, num, kind, activity_state, activity_last_at, is_terminated, created_at, updated_at)
			VALUES (?, 'optimus_db', ?, 'orchestrator', 'idle', ?, 0, ?, ?)`, id, n+1, created, created, created); err != nil {
			t.Fatalf("seed session %s: %v", id, err)
		}
	}

	upTo(t, db, autoResumeMigration)

	rows, err := db.Query(`SELECT session_id, policy, stop_reason FROM session_auto_resume ORDER BY session_id`)
	if err != nil {
		t.Fatalf("query policies: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var got []string
	for rows.Next() {
		var id, policy, reason string
		if err := rows.Scan(&id, &policy, &reason); err != nil {
			t.Fatal(err)
		}
		got = append(got, id+"="+policy+"/"+reason)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []string{"optimus_db-1=never/", "optimus_rails_app-1=never/"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("seeded policies = %v, want exactly %v (every other session stays allowed by having no row)", got, want)
	}
}

func TestAutoResumeMigrationIsANoOpWhereTheNamedSessionsDoNotExist(t *testing.T) {
	db := openMigratedDatabaseCopy(t, 20261026004000000)
	upTo(t, db, autoResumeMigration)
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM session_auto_resume`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("a fresh database must seed nothing: n=%d err=%v", n, err)
	}
	for _, table := range []string{"daemon_runs", "session_resume_intents"} {
		if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s must start empty: n=%d err=%v", table, n, err)
		}
	}
}

func TestAutoResumeTablesRejectBadValuesAndFollowTheSessionsLifetime(t *testing.T) {
	db := openMigratedDatabaseCopy(t, 20261026004000000)
	created := time.Date(2026, 10, 22, 21, 0, 0, 0, time.UTC)
	if _, err := db.Exec(`INSERT INTO projects (id, path, display_name, registered_at) VALUES ('p', '/tmp/p', 'p', ?)`, created); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sessions (id, project_id, num, kind, activity_state, activity_last_at, is_terminated, created_at, updated_at)
		VALUES ('p-1', 'p', 1, 'worker', 'idle', ?, 0, ?, ?)`, created, created, created); err != nil {
		t.Fatal(err)
	}
	upTo(t, db, autoResumeMigration)
	if _, err := db.Exec(`INSERT INTO session_auto_resume (session_id, policy, updated_at) VALUES ('p-1', 'maybe', ?)`, created); err == nil {
		t.Fatal("an unknown policy passed the CHECK")
	}
	if _, err := db.Exec(`INSERT INTO session_auto_resume (session_id, policy, stop_reason, updated_at) VALUES ('p-1', 'allowed', 'because', ?)`, created); err == nil {
		t.Fatal("an unknown stop reason passed the CHECK")
	}
	if _, err := db.Exec(`INSERT INTO session_resume_intents (session_id, record) VALUES ('p-1', 'not json')`); err == nil {
		t.Fatal("a non-JSON intent record passed the CHECK")
	}
	if _, err := db.Exec(`INSERT INTO session_auto_resume (session_id, policy, updated_at) VALUES ('p-1', 'never', ?)`, created); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO session_resume_intents (session_id, record) VALUES ('p-1', '{}')`); err != nil {
		t.Fatal(err)
	}
	// Both tables follow the session's lifetime. Deleting a session row is
	// restricted by unrelated tables, so assert the foreign keys directly.
	for _, table := range []string{"session_auto_resume", "session_resume_intents"} {
		if !cascadesFromSessions(t, db, table) {
			t.Fatalf("%s must reference sessions ON DELETE CASCADE", table)
		}
	}
}

func cascadesFromSessions(t *testing.T, db *sql.DB, table string) bool {
	t.Helper()
	rows, err := db.Query(`SELECT "table", on_delete FROM pragma_foreign_key_list(?)`, table)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	found := false
	for rows.Next() {
		var parent, onDelete string
		if err := rows.Scan(&parent, &onDelete); err != nil {
			t.Fatal(err)
		}
		found = found || (parent == "sessions" && onDelete == "CASCADE")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return found
}
