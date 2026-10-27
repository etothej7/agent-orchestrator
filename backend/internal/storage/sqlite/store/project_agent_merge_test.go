package store_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

func agentMergeProject(t *testing.T) (*sqlite.Store, domain.ProjectRecord) {
	t.Helper()
	store := sqlitetest.MustOpen(t)
	rec := domain.ProjectRecord{ID: "demo", Path: t.TempDir(), RegisteredAt: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC), Config: domain.ProjectConfig{AgentRules: "a"}}
	if err := store.UpsertProject(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	return store, rec
}

func mayMerge(t *testing.T, store *sqlite.Store) bool {
	t.Helper()
	got, ok, err := store.GetProject(context.Background(), "demo")
	if err != nil || !ok {
		t.Fatalf("get project: %v %v", ok, err)
	}
	return got.Config.AgentMayMerge
}

// Only SetProjectAgentMayMerge changes the setting. Every other writer carries whatever copy of the config it
// has, which may be stale (read before the person changed it) or hostile, and none of them can flip it.
func TestOnlySetProjectAgentMayMergeChangesWhetherAgentsMayMerge(t *testing.T) {
	ctx := context.Background()
	store, rec := agentMergeProject(t)
	staleOff := rec // a copy of the project read while the setting was off
	staleOn := rec
	staleOn.Config.AgentMayMerge = true

	// A stale or hostile writer cannot turn it on...
	for name, write := range map[string]func() error{
		"UpsertProject": func() error { return store.UpsertProject(ctx, staleOn) },
		"UpdateProjectSettings": func() error {
			_, err := store.UpdateProjectSettings(ctx, "demo", "x", staleOn.Config)
			return err
		},
	} {
		if err := write(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if mayMerge(t, store) {
			t.Fatalf("%s turned agentMayMerge on", name)
		}
	}

	// ...the person's write does...
	if ok, err := store.SetProjectAgentMayMerge(ctx, "demo", true); err != nil || !ok || !mayMerge(t, store) {
		t.Fatalf("SetProjectAgentMayMerge(true) = %v %v, now %v", ok, err, mayMerge(t, store))
	}
	// ...and then a writer holding the old copy (setting off) cannot turn it back off, nor change what else was saved.
	staleOff.Config.AgentRules = "rewritten"
	for name, write := range map[string]func() error{
		"UpsertProject": func() error { return store.UpsertProject(ctx, staleOff) },
		"UpdateProjectSettings": func() error {
			_, err := store.UpdateProjectSettings(ctx, "demo", "x", staleOff.Config)
			return err
		},
	} {
		if err := write(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got, _, _ := store.GetProject(ctx, "demo")
		if !got.Config.AgentMayMerge || got.Config.AgentRules != "rewritten" {
			t.Fatalf("%s: agentMayMerge=%v agentRules=%q; the setting must be kept and the rest written", name, got.Config.AgentMayMerge, got.Config.AgentRules)
		}
	}

	if ok, err := store.SetProjectAgentMayMerge(ctx, "demo", false); err != nil || !ok || mayMerge(t, store) {
		t.Fatalf("SetProjectAgentMayMerge(false) = %v %v, now %v", ok, err, mayMerge(t, store))
	}
	if ok, err := store.SetProjectAgentMayMerge(ctx, "missing", true); err != nil || ok {
		t.Fatalf("a missing project: %v %v", ok, err)
	}
}

// The interleaving that matters: writers racing the person. Whatever order they run in, once the person has set the
// final value no writer can leave a different one behind.
func TestWritersRacingThePersonNeverLeaveADifferentAgentMayMerge(t *testing.T) {
	ctx := context.Background()
	for _, final := range []bool{true, false} {
		store, rec := agentMergeProject(t)
		if _, err := store.SetProjectAgentMayMerge(ctx, "demo", !final); err != nil {
			t.Fatal(err)
		}
		opposite := rec
		opposite.Config.AgentMayMerge = !final // what a stale or hostile writer carries
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < 20; j++ {
					_ = store.UpsertProject(ctx, opposite)
					_, _ = store.UpdateProjectSettings(ctx, "demo", "n", opposite.Config)
				}
			}()
		}
		if _, err := store.SetProjectAgentMayMerge(ctx, "demo", final); err != nil {
			t.Fatal(err)
		}
		wg.Wait()
		if got := mayMerge(t, store); got != final {
			t.Fatalf("final = %v after racing writers, want the person's %v", got, final)
		}
	}
}
