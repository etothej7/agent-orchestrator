package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

func TestCoordinatorStartupNoticeFencesReplacementOwnerAndCancellation(t *testing.T) {
	ctx := context.Background()
	db := sqlitetest.MustOpen(t)
	seedProject(t, db, "coordinator")
	now := time.Now()
	if err := db.SetCoordination(ctx, domain.CoordinationConfig{CoordinatorProjectID: "coordinator"}, now); err != nil {
		t.Fatal(err)
	}
	rec := sampleRecord("coordinator")
	rec.Kind = domain.KindOrchestrator
	rec.Mode = domain.SessionModeChat
	old, err := db.CreateSession(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	conv, err := db.CreateConversation(ctx, "main", domain.ConversationScopeProject, "coordinator", old.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	current, err := db.CreateSession(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.CreateProjectConversationWithContextReset(ctx, "unused", "coordinator", current.ID, domain.ConversationActivity{ID: "reset", Kind: domain.ActivityKindSystem, Status: domain.ActivityStatusCompleted, Summary: "Context reset.", ProviderItemID: domain.ConversationContextResetProviderItemID(current.ID)}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.SetSessionProvisionState(ctx, old.ID, domain.SessionProvisionFailed, "old failure", now); err != nil {
		t.Fatal(err)
	}
	if err = db.RecordCoordinatorStartupFailure(ctx, old.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err = db.SetSessionProvisionCancelled(ctx, current.ID, now); err != nil {
		t.Fatal(err)
	}
	if err = db.RecordCoordinatorStartupFailure(ctx, current.ID, now); err != nil {
		t.Fatal(err)
	}
	marks, err := db.CoordinatorHumanMessages(ctx, conv.ID)
	if err != nil || len(marks) != 0 {
		t.Fatal("stale owner or cancellation published", marks, err)
	}
	if _, err = db.SetSessionProvisionState(ctx, current.ID, domain.SessionProvisionProvisioning, "", now); err != nil {
		t.Fatal(err)
	}
	if _, err = db.SetSessionProvisionState(ctx, current.ID, domain.SessionProvisionFailed, "new failure", now); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = db.RecordCoordinatorStartupFailure(ctx, current.ID, now); err != nil {
			t.Fatal(err)
		}
	}
	marks, err = db.CoordinatorHumanMessages(ctx, conv.ID)
	if err != nil || len(marks) != 1 {
		t.Fatal("current genuine failure not exactly once", marks, err)
	}
}
