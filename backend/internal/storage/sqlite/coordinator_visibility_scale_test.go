package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	sqlstore "github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// seedCoordinatorConversation fills a coordinator conversation with turns that
// alternate between Ed's requests and automation, each with a reply, reasoning
// and (for Ed's turns) a steer, in one transaction.
func seedCoordinatorConversation(t *testing.T, db *sql.DB, conversation string, turns int) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	var branch string
	if err := tx.QueryRow(`SELECT active_branch_id FROM conversations WHERE id=?`, conversation).Scan(&branch); err != nil {
		t.Fatal(err)
	}
	seq := 0
	for i := 0; i < turns; i++ {
		turn := fmt.Sprintf("turn-%d", i)
		human := i%2 == 0
		if _, err := tx.Exec(`INSERT INTO conversation_turns(id,conversation_id,handled_by_session_id,state,requested_at,branch_id) VALUES(?,?,?,?,CURRENT_TIMESTAMP,?)`, turn, conversation, "coord-1", "completed", branch); err != nil {
			t.Fatal(err)
		}
		origin, client := "automation", fmt.Sprintf("report-batch:%d", i)
		if human {
			origin, client = "human", fmt.Sprintf("typed-%d", i)
		}
		seq++
		user := fmt.Sprintf("user-%d", i)
		if _, err := tx.Exec(`INSERT INTO conversation_messages(id,conversation_id,turn_id,sequence,role,origin,text,client_message_id,branch_id,created_at,updated_at) VALUES(?,?,?,?,'user',?,'request',?,?,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`, user, conversation, turn, seq, origin, client, branch); err != nil {
			t.Fatal(err)
		}
		if human {
			if _, err := tx.Exec(`INSERT INTO coordinator_human_messages(message_id,purpose) VALUES(?,'human_request')`, user); err != nil {
				t.Fatal(err)
			}
		}
		seq++
		if _, err := tx.Exec(`INSERT INTO conversation_activities(id,conversation_id,turn_id,sequence,kind,status,summary,detail_json,branch_id,created_at,updated_at) VALUES(?,?,?,?,'reasoning','completed','thinking','',?,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`, fmt.Sprintf("act-%d", i), conversation, turn, seq, branch); err != nil {
			t.Fatal(err)
		}
		seq++
		if _, err := tx.Exec(`INSERT INTO conversation_messages(id,conversation_id,turn_id,sequence,role,origin,text,branch_id,created_at,updated_at) VALUES(?,?,?,?,'assistant','provider','reply',?,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`, fmt.Sprintf("reply-%d", i), conversation, turn, seq, branch); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(`UPDATE conversations SET latest_sequence=? WHERE id=?`, seq, conversation); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// Reply visibility must stay linear in the conversation: Ed's coordinator
// conversation is long-lived and its snapshot is polled. A correlated lookup per
// assistant message would be quadratic and show up as the ratio below.
func TestCoordinatorVisibilityQueriesScaleLinearly(t *testing.T) {
	ctx := context.Background()
	measure := func(turns int) (messages, page time.Duration, labels, visible int) {
		// Rows reference sessions and turns this synthetic data does not need.
		db := openMigratedDatabaseCopyNoForeignKeys(t, 20261026003100000)
		mustExec(t, db, `INSERT INTO projects(id,path,registered_at) VALUES('coord','/tmp/coord',CURRENT_TIMESTAMP)`)
		mustExec(t, db, `INSERT INTO conversations(id,scope,project_id,active_branch_id,created_at,updated_at) VALUES('scale','project','coord','scale:root',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`)
		mustExec(t, db, `INSERT INTO conversation_branches(id,conversation_id,created_at) VALUES('scale:root','scale',CURRENT_TIMESTAMP)`)
		st := sqlstore.NewStore(db, db)
		seedCoordinatorConversation(t, db, "scale", turns)
		best := func(f func()) time.Duration {
			d := time.Duration(1<<62 - 1)
			for range 3 {
				start := time.Now()
				f()
				d = min(d, time.Since(start))
			}
			return d
		}
		messages = best(func() {
			marks, err := st.CoordinatorHumanMessages(ctx, "scale")
			if err != nil {
				t.Fatal(err)
			}
			labels = len(marks)
		})
		page = best(func() {
			rows, err := st.LoadCoordinatorHumanSnapshotPage(ctx, "scale", 0, 500)
			if err != nil {
				t.Fatal(err)
			}
			visible = len(rows.Messages)
		})
		return
	}
	smallTurns, largeTurns := 500, 2500 // the large conversation has 5,000 messages
	if raceDetector {
		smallTurns, largeTurns = 200, 1000 // same 5x ratio, seeded fast enough under the race detector
	}
	smallM, smallP, smallLabels, smallVisible := measure(smallTurns)
	largeM, largeP, largeLabels, largeVisible := measure(largeTurns)
	t.Logf("%d turns: labels=%d page=%d messages in %v / %v; %d turns: labels=%d page=%d in %v / %v", smallTurns, smallLabels, smallVisible, smallM, smallP, largeTurns, largeLabels, largeVisible, largeM, largeP)
	// Ed's turns: request + reply visible; automation turns: nothing.
	if smallLabels != smallTurns || largeLabels != largeTurns {
		t.Fatal("unexpected visibility", smallLabels, largeLabels)
	}
	if largeVisible != 500 {
		t.Fatal("page was not bounded to the newest visible items", largeVisible)
	}
	// The absolute bound is for normal builds; the race detector slows queries
	// several-fold, so under it only the scaling ratio below is checked.
	if !raceDetector && (largeM > 1500*time.Millisecond || largeP > 1500*time.Millisecond) {
		t.Fatalf("5k-message conversation too slow: %v / %v", largeM, largeP)
	}
	// 5x the data should cost about 5x; a quadratic plan costs about 25x.
	if floor := 2 * time.Millisecond; largeM > 12*max(smallM, floor) || largeP > 12*max(smallP, floor) {
		t.Fatalf("visibility cost is not linear: messages %v -> %v, page %v -> %v", smallM, largeM, smallP, largeP)
	}
}
