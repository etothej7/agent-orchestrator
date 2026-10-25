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
	if !strings.Contains(text, "Earlier Ed request") || !strings.Contains(text, "Earlier public assistant answer") || strings.Contains(text, "PRIVATE_") || len(public.Activities) != 0 {
		t.Fatal(text, public.Activities)
	}
	if len(rows.Messages) != 3 || len(rows.Activities) != 1 {
		t.Fatal("durable source history changed", rows)
	}
	var integrity string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatal(integrity, err)
	}
}
