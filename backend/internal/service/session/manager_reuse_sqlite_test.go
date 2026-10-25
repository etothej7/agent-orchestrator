package session

import (
	"context"
	"sync"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestManagerCreationSupportedPathReusesDurableActorAcrossRetries(t *testing.T) {
	st := newSQLiteKanbanTestStore(t)
	seedKanbanTestProject(t, st, "managed")
	ctx := context.Background()
	fc := &fakeCommander{}
	fc.spawnFunc = func(cfg ports.SpawnConfig) domain.SessionRecord {
		rec, err := st.CreateSession(ctx, domain.SessionRecord{ProjectID: cfg.ProjectID, Kind: domain.KindOrchestrator, Harness: domain.HarnessCodex, Mode: domain.SessionModeChat})
		if err != nil {
			t.Error(err)
		}
		return rec
	}
	svc := NewWithDeps(Deps{Store: st, Manager: fc})
	cfg := ports.SpawnConfig{ProjectID: "managed", Kind: domain.KindOrchestrator, Harness: domain.HarnessCodex, RequestedMode: domain.SessionModeChat, DisplayName: "Repo manager"}
	var wg sync.WaitGroup
	ids := make(chan domain.SessionID, 2)
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); s, _, _, err := svc.Spawn(ctx, cfg); ids <- s.ID; errs <- err }()
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var id domain.SessionID
	for got := range ids {
		if id != "" && id != got {
			t.Fatal("duplicate managers", id, got)
		}
		id = got
	}
	retry := NewWithDeps(Deps{Store: st, Manager: fc})
	got, _, _, err := retry.Spawn(ctx, cfg)
	if err != nil || got.ID != id || fc.spawnCalls != 1 {
		t.Fatal("retry did not reuse durable manager", got, fc.spawnCalls, err)
	}
	rows, err := st.ListSessions(ctx, "managed")
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
	// Worker-only idempotency keys are not falsely advertised for this route.
	cfg.ClientRequestID = "unsupported-manager-key"
	if _, _, _, err := retry.Spawn(ctx, cfg); err == nil {
		t.Fatal("keyed manager unexpectedly accepted")
	}
}
