package coordination_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/coordination"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/project"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/report"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

func setup(t *testing.T) (*sqlite.Store, *coordination.Service, domain.CoordinationConfig, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	svc := coordination.New(s, dir)
	id, err := svc.CreateProject(context.Background(), domain.HarnessCodex)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"api", "web", "unmanaged"} {
		if err := s.UpsertProject(context.Background(), domain.ProjectRecord{ID: id, Path: filepath.Join(dir, id), DisplayName: id, RegisteredAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	cfg := domain.CoordinationConfig{CoordinatorProjectID: id, ManagedProjectIDs: []domain.ProjectID{"api", "web"}}
	if _, err := svc.Set(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	return s, svc, cfg, dir
}

func session(t *testing.T, s *sqlite.Store, project domain.ProjectID, kind domain.SessionKind) domain.SessionRecord {
	t.Helper()
	now := time.Now().UTC()
	r, err := s.CreateSession(context.Background(), domain.SessionRecord{ProjectID: project, Kind: kind, Harness: domain.HarnessFake, Mode: domain.SessionModeChat, CreatedAt: now, UpdatedAt: now, Activity: domain.Activity{State: domain.ActivityIdle, LastActivityAt: now}})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestAssignmentsValidateAndPreserveLiveRoles(t *testing.T) {
	s, svc, cfg, dir := setup(t)
	if _, err := os.Stat(filepath.Join(dir, "coordination", ".git")); !os.IsNotExist(err) {
		t.Fatalf("coordinator must not initialize Git: %v", err)
	}
	for _, invalid := range []domain.CoordinationConfig{
		{ManagedProjectIDs: []domain.ProjectID{"api"}},
		{CoordinatorProjectID: "api"},
		{CoordinatorProjectID: cfg.CoordinatorProjectID, ManagedProjectIDs: []domain.ProjectID{"missing"}},
		{CoordinatorProjectID: cfg.CoordinatorProjectID, ManagedProjectIDs: []domain.ProjectID{"api", "api"}},
		{CoordinatorProjectID: cfg.CoordinatorProjectID, ManagedProjectIDs: []domain.ProjectID{cfg.CoordinatorProjectID}},
	} {
		if _, err := svc.Set(context.Background(), invalid); err == nil {
			t.Fatalf("accepted invalid assignment: %+v", invalid)
		}
	}
	manager := session(t, s, "api", domain.KindOrchestrator)
	if _, err := svc.Set(context.Background(), cfg); err != nil {
		t.Fatalf("same assignments should be idempotent: %v", err)
	}
	if _, err := svc.Set(context.Background(), domain.CoordinationConfig{}); err == nil {
		t.Fatal("changed live manager role")
	}
	manager.IsTerminated = true
	if err := s.UpdateSession(context.Background(), manager); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Set(context.Background(), domain.CoordinationConfig{}); err != nil {
		t.Fatal(err)
	}
}

type delivery struct {
	targets  []domain.SessionID
	messages []string
	fail     bool
}

func (d *delivery) Submit(_ context.Context, id domain.SessionID, text, _ string) error {
	if d.fail {
		return errors.New("temporarily unavailable")
	}
	d.targets = append(d.targets, id)
	d.messages = append(d.messages, text)
	return nil
}
func (*delivery) Interrupt(context.Context, domain.SessionID) error { return nil }

func TestHierarchyRoutesSeparatelyAndRecoversAcrossRestart(t *testing.T) {
	ctx := context.Background()
	s, svc, cfg, dir := setup(t)
	apiManager := session(t, s, "api", domain.KindOrchestrator)
	webManager := session(t, s, "web", domain.KindOrchestrator)
	worker := session(t, s, "api", domain.KindWorker)
	writer := report.New(report.Deps{Store: s, Coordination: s.GetCoordination})
	workerReport, err := writer.Create(ctx, report.CreateInput{SessionID: worker.ID, State: domain.ReportNeedsInput, Note: "worker needs API direction"})
	if err != nil {
		t.Fatal(err)
	}
	apiReport, err := writer.Create(ctx, report.CreateInput{SessionID: apiManager.ID, Message: "API suggestion"})
	if err != nil {
		t.Fatal(err)
	}
	webReport, err := writer.Create(ctx, report.CreateInput{SessionID: webManager.ID, Message: "Web suggestion"})
	if err != nil {
		t.Fatal(err)
	}
	if workerReport.DeliveryProjectID() != "api" || apiReport.ProjectID != "api" || apiReport.DeliveryProjectID() != cfg.CoordinatorProjectID || webReport.ProjectID != "web" {
		t.Fatal("source and recipient were conflated")
	}
	d := &delivery{}
	c := report.NewCoordinator(report.CoordinatorDeps{Store: s, Delivery: d})
	if _, err := c.RunDue(ctx); err == nil {
		t.Fatal("missing overall coordinator should remain retryable")
	}
	if len(d.targets) != 1 || d.targets[0] != apiManager.ID {
		t.Fatalf("offline overall coordinator blocked worker delivery: %v", d.targets)
	}
	// End managers to isolate the pending-report assignment guard.
	for _, manager := range []domain.SessionRecord{apiManager, webManager} {
		manager.IsTerminated = true
		if err := s.UpdateSession(ctx, manager); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.Set(ctx, domain.CoordinationConfig{}); err == nil {
		t.Fatal("discarded assignments with pending manager reports")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	persisted, err := reopened.GetCoordination(ctx)
	if err != nil || persisted.CoordinatorProjectID != cfg.CoordinatorProjectID {
		t.Fatalf("lost mapping: %+v %v", persisted, err)
	}
	overall := session(t, reopened, cfg.CoordinatorProjectID, domain.KindOrchestrator)
	d = &delivery{fail: true}
	c = report.NewCoordinator(report.CoordinatorDeps{Store: reopened, Delivery: d})
	if _, err := c.RunDue(ctx); err == nil {
		t.Fatal("delivery failure disappeared")
	}
	d.fail = false
	// Chat piggyback must claim by recipient, not by either source repo.
	batch, err := c.PreparePiggyback(ctx, overall.ID)
	if err != nil || len(batch.Reports) != 2 {
		t.Fatalf("piggyback = %+v, %v", batch, err)
	}
	if !strings.Contains(batch.Render(), "ao://sessions/api/") || !strings.Contains(batch.Render(), "ao://sessions/web/") {
		t.Fatal("manager source attribution lost")
	}
	rejected := errors.New("turn rejected")
	if err := c.ReleasePiggyback(ctx, batch, rejected); !errors.Is(err, rejected) {
		t.Fatal(err)
	}
	if _, err := c.RunDue(ctx); err != nil {
		t.Fatal(err)
	}
	if len(d.targets) != 1 || d.targets[0] != overall.ID {
		t.Fatalf("wrong retry destination: %v", d.targets)
	}
	if _, err := c.RunDue(ctx); err != nil || len(d.targets) != 1 {
		t.Fatal("acknowledged batch was delivered twice")
	}
}

func TestOnlyCurrentAssignedManagersCanReportUpward(t *testing.T) {
	s, _, cfg, _ := setup(t)
	writer := report.New(report.Deps{Store: s, Coordination: s.GetCoordination})
	old := session(t, s, "api", domain.KindOrchestrator)
	current := session(t, s, "api", domain.KindOrchestrator)
	unmanaged := session(t, s, "unmanaged", domain.KindOrchestrator)
	overall := session(t, s, cfg.CoordinatorProjectID, domain.KindOrchestrator)
	for _, id := range []domain.SessionID{old.ID, unmanaged.ID, overall.ID} {
		if _, err := writer.Create(context.Background(), report.CreateInput{SessionID: id, Message: "suggestion"}); err == nil {
			t.Fatalf("accepted reporter %s", id)
		}
	}
	if _, err := writer.Create(context.Background(), report.CreateInput{SessionID: current.ID, Message: "suggestion"}); err != nil {
		t.Fatal(err)
	}
	current.IsTerminated = true
	if err := s.UpdateSession(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Create(context.Background(), report.CreateInput{SessionID: current.ID, Message: "suggestion"}); err == nil {
		t.Fatal("accepted terminated manager")
	}
}

type pausedReportStore struct {
	*sqlite.Store
	read, proceed chan struct{}
}

func (s *pausedReportStore) GetSession(ctx context.Context, id domain.SessionID) (domain.SessionRecord, bool, error) {
	rec, found, err := s.Store.GetSession(ctx, id)
	close(s.read)
	<-s.proceed
	return rec, found, err
}

func TestAssignmentChangeCannotOvertakeInFlightReport(t *testing.T) {
	s, svc, _, _ := setup(t)
	manager := session(t, s, "api", domain.KindOrchestrator)
	paused := &pausedReportStore{Store: s, read: make(chan struct{}), proceed: make(chan struct{})}
	writer := report.New(report.Deps{Store: paused, Coordination: s.GetCoordination})
	done := make(chan error, 1)
	go func() {
		_, err := writer.Create(context.Background(), report.CreateInput{SessionID: manager.ID, Message: "Final suggestion"})
		done <- err
	}()
	<-paused.read // report holds the shared gate but has not inserted its row
	manager.IsTerminated = true
	if err := s.UpdateSession(context.Background(), manager); err != nil {
		t.Fatal(err)
	}
	_, blocked := svc.Set(context.Background(), domain.CoordinationConfig{})
	close(paused.proceed)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if blocked == nil {
		t.Fatal("assignment change overtook an in-flight report")
	}
	if _, err := svc.Set(context.Background(), domain.CoordinationConfig{}); err == nil {
		t.Fatal("assignment change discarded the now-pending report")
	}
}

type teardown struct{ called bool }

func (t *teardown) TeardownProject(context.Context, domain.ProjectID) error {
	t.called = true
	return nil
}

func TestAssignedProjectsCannotBeRemovedBeforeTeardown(t *testing.T) {
	s, _, cfg, _ := setup(t)
	manager := session(t, s, "api", domain.KindOrchestrator)
	writer := report.New(report.Deps{Store: s, Coordination: s.GetCoordination})
	if _, err := writer.Create(context.Background(), report.CreateInput{SessionID: manager.ID, Message: "Pending suggestion"}); err != nil {
		t.Fatal(err)
	}
	teardown := &teardown{}
	projects := project.NewWithDeps(project.Deps{Store: s, Sessions: teardown})
	for _, id := range []domain.ProjectID{cfg.CoordinatorProjectID, "api"} {
		if _, err := projects.Remove(context.Background(), id); err == nil {
			t.Fatalf("removed assigned project %s", id)
		}
		row, _, _ := s.GetProject(context.Background(), string(id))
		if teardown.called || !row.ArchivedAt.IsZero() {
			t.Fatal("removal had side effects before rejecting assigned project")
		}
	}
	if _, err := projects.Remove(context.Background(), "unmanaged"); err != nil || !teardown.called {
		t.Fatalf("ordinary removal failed: %v", err)
	}
}

func TestDedicatedCoordinatorCanBeRecreatedAfterRemoval(t *testing.T) {
	s, svc, cfg, _ := setup(t)
	if _, err := svc.Set(context.Background(), domain.CoordinationConfig{}); err != nil {
		t.Fatal(err)
	}
	if _, err := project.New(s).Remove(context.Background(), cfg.CoordinatorProjectID); err != nil {
		t.Fatal(err)
	}
	id, err := svc.CreateProject(context.Background(), domain.HarnessCodex)
	if err != nil || id != cfg.CoordinatorProjectID {
		t.Fatalf("recreate = %s, %v", id, err)
	}
	if _, err := svc.Set(context.Background(), cfg); err != nil {
		t.Fatalf("recreated coordinator is unavailable: %v", err)
	}
}
