package sqlite

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	sqlstore "github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

func TestCoordinatorAudienceUpgradePreservesExistingPublicHistory(t *testing.T) {
	// A real copied pre-audience database, not a fixture pretending to be migrated.
	db := openMigratedDatabaseCopy(t, 20261025200310086)
	mustExec(t, db, `INSERT INTO projects(id,path,registered_at) VALUES('history','/tmp/history',CURRENT_TIMESTAMP)`)
	mustExec(t, db, `INSERT INTO conversations(id,scope,project_id,created_at,updated_at) VALUES('history-conv','project','history',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`)
	mustExec(t, db, `INSERT INTO conversation_messages(id,conversation_id,sequence,role,origin,text,client_message_id,created_at,updated_at) VALUES
 ('old-user','history-conv',1,'user','human','Earlier Ed request','old-client',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP),
 ('old-assistant','history-conv',2,'assistant','provider','Earlier public assistant answer','',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`)
	// Ed's session-opening prompt predates client ids; a provider import has a
	// provider item and also no client id.
	mustExec(t, db, `INSERT INTO conversation_messages(id,conversation_id,sequence,role,origin,text,client_message_id,provider_item_id,created_at,updated_at) VALUES
 ('opening-prompt','history-conv',-2,'user','human','Ed opening prompt without client id','','',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP),
 ('provider-import','history-conv',-1,'user','human','PRIVATE_PROVIDER_IMPORT','','imported-item',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`)
	if err := migrate(db); err != nil {
		t.Fatal(err)
	}
	// Newly generated chatter must not inherit the historical exception.
	mustExec(t, db, `INSERT INTO conversation_messages(id,conversation_id,sequence,role,origin,text,created_at,updated_at) VALUES('new-ack','history-conv',3,'assistant','provider','PRIVATE_NEW_ACK',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`)
	mustExec(t, db, `INSERT INTO conversation_activities(id,conversation_id,sequence,kind,status,summary,created_at,updated_at) VALUES('reasoning','history-conv',4,'reasoning','completed','PRIVATE_REASONING',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`)
	st := sqlstore.NewStore(db, db)
	ctx := context.Background()
	if err := st.SetCoordination(ctx, domain.CoordinationConfig{CoordinatorProjectID: "history"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	rows, err := st.LoadConversationSnapshot(ctx, "history-conv")
	if err != nil {
		t.Fatal(err)
	}
	svc := chatsvc.New(chatsvc.Options{Store: st, Sessions: st})
	public, err := svc.HumanSnapshot(ctx, chatsvc.Snapshot{Conversation: rows.Conversation, Messages: rows.Messages, Activities: rows.Activities})
	if err != nil {
		t.Fatal(err)
	}
	text := ""
	for _, msg := range public.Messages {
		text += msg.Text + "\n"
	}
	if !strings.Contains(text, "Ed opening prompt without client id") || !strings.Contains(text, "Earlier Ed request") || !strings.Contains(text, "Earlier public assistant answer") || strings.Contains(text, "PRIVATE_") || len(public.Activities) != 0 {
		t.Fatal(text, public.Activities)
	}
	if len(rows.Messages) != 5 || len(rows.Activities) != 1 {
		t.Fatal("durable source history changed", rows)
	}
	var integrity string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatal(integrity, err)
	}
}

// An install that already ran the first coordinator-audience build (66b827b9d)
// has the old table. The forward migrations must widen it without losing a
// row, label the opening prompt, and leave ao tell storable.
func TestCoordinatorAudienceUpgradeFromFirstBuildWidensPurposesAndBackfills(t *testing.T) {
	// 20261025220228420 is the last migration the first build shipped.
	db := openMigratedDatabaseCopy(t, 20261025220228420)
	mustExec(t, db, `INSERT INTO projects(id,path,registered_at) VALUES('coord','/tmp/coord',CURRENT_TIMESTAMP)`)
	mustExec(t, db, `INSERT INTO conversations(id,scope,project_id,created_at,updated_at) VALUES('coord-conv','project','coord',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`)
	mustExec(t, db, `INSERT INTO conversation_messages(id,conversation_id,sequence,role,origin,text,client_message_id,provider_item_id,created_at,updated_at) VALUES
 ('opening','coord-conv',1,'user','human','Opening prompt','','',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP),
 ('import','coord-conv',2,'user','human','Imported','','item-1',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP),
 ('typed','coord-conv',3,'user','human','Typed by Ed','client-1','',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP),
 ('reply','coord-conv',4,'assistant','provider','Old reply','','',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP),
 ('decision','coord-conv',5,'assistant','daemon','Question','','',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`)
	for id, purpose := range map[string]string{"typed": "human_request", "reply": "legacy_public", "decision": "scope_question"} {
		mustExec(t, db, `INSERT INTO coordinator_human_messages(message_id,purpose) VALUES(?,?)`, id, purpose)
	}
	// The old table refuses the new purposes: this is the failure being fixed.
	if _, err := db.Exec(`INSERT INTO coordinator_human_messages(message_id,purpose) VALUES('typed','verified_result')`); err == nil {
		t.Fatal("precondition: the first build's table should reject verified_result")
	}
	if err := migrate(db); err != nil {
		t.Fatal(err)
	}
	labels := map[string]string{}
	rows, err := db.Query(`SELECT message_id, purpose FROM coordinator_human_messages`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, purpose string
		if err := rows.Scan(&id, &purpose); err != nil {
			t.Fatal(err)
		}
		labels[id] = purpose
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"typed": "human_request", "reply": "legacy_public", "decision": "scope_question", "opening": "human_request"}
	if len(labels) != len(want) {
		t.Fatal("labels", labels)
	}
	for id, purpose := range want {
		if labels[id] != purpose {
			t.Fatalf("label %s = %q, want %q (labels %v)", id, labels[id], purpose, labels)
		}
	}
	if _, found := labels["import"]; found {
		t.Fatal("provider import was labeled as human")
	}
	// Both ao tell purposes and the new notification type are storable.
	mustExec(t, db, `INSERT INTO conversation_messages(id,conversation_id,sequence,role,origin,text,created_at,updated_at) VALUES('n1','coord-conv',6,'assistant','daemon','Verified',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP),('n2','coord-conv',7,'assistant','daemon','Stopped',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`)
	mustExec(t, db, `INSERT INTO coordinator_human_messages(message_id,purpose) VALUES('n1','verified_result'),('n2','stopped_work')`)
	var indexed int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_conversation_messages_turn'`).Scan(&indexed); err != nil || indexed != 1 {
		t.Fatal("turn index missing", indexed, err)
	}
	var integrity string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatal(integrity, err)
	}
}
