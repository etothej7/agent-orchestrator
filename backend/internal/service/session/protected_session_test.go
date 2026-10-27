package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	sessionmanager "github.com/aoagents/agent-orchestrator/backend/internal/session_manager"
)

func protectedProject() (*fakeStore, *fakeCommander) {
	st := newFakeStore()
	st.projects["mer"] = domain.ProjectRecord{ID: "mer"}
	st.sessions["mer-1"] = domain.SessionRecord{ID: "mer-1", ProjectID: "mer", Kind: domain.KindOrchestrator}
	st.sessions["mer-2"] = domain.SessionRecord{ID: "mer-2", ProjectID: "mer", Kind: domain.KindOrchestrator}
	st.sessions["mer-3"] = domain.SessionRecord{ID: "mer-3", ProjectID: "mer", Kind: domain.KindWorker}
	return st, &fakeCommander{protectedSessions: map[domain.SessionID]bool{"mer-2": true}}
}

func requireProtected(t *testing.T, err error) {
	t.Helper()
	var apiErr *apierr.Error
	if !errors.As(err, &apiErr) || apiErr.Kind != apierr.KindConflict || apiErr.Code != "SESSION_PROTECTED" {
		t.Fatalf("error = %v, want a 409 SESSION_PROTECTED naming how to unlock", err)
	}
}

// A clean replacement retires every existing orchestrator, and retirement force-removes
// even a dirty worktree. One protected orchestrator must refuse the whole replacement
// BEFORE any is retired, so the project is never left half-retired.
func TestSpawnOrchestratorCleanRefusesWhenAnyOrchestratorIsProtected(t *testing.T) {
	st, fc := protectedProject()
	svc := &Service{manager: fc, store: st}

	_, err := svc.SpawnOrchestrator(context.Background(), "mer", true, "", "")
	requireProtected(t, err)
	if len(fc.retired) != 0 || len(fc.sent) != 0 || fc.spawned {
		t.Fatalf("nothing may happen before the refusal: retired=%v notices=%v spawned=%v", fc.retired, fc.sent, fc.spawned)
	}
}

func TestSpawnOrchestratorCleanStillWorksWhenNothingIsProtected(t *testing.T) {
	st, fc := protectedProject()
	fc.protectedSessions = nil
	svc := &Service{manager: fc, store: st}

	if _, err := svc.SpawnOrchestrator(context.Background(), "mer", true, "", ""); err != nil {
		t.Fatalf("SpawnOrchestrator: %v", err)
	}
	if len(fc.retired) != 2 || !fc.spawned {
		t.Fatalf("control: retired=%v spawned=%v", fc.retired, fc.spawned)
	}
}

func TestTeardownProjectRefusesWhileAnyLiveSessionIsProtectedAndKillsNothing(t *testing.T) {
	st, fc := protectedProject()
	svc := &Service{manager: fc, store: st}

	err := svc.TeardownProject(context.Background(), "mer")
	requireProtected(t, err)
	if len(fc.killed) != 0 || len(fc.cleanupProjects) != 0 {
		t.Fatalf("teardown must refuse before killing anything: killed=%v cleanup=%v", fc.killed, fc.cleanupProjects)
	}
}

func TestTeardownProjectIgnoresAProtectedSessionThatIsAlreadyTerminated(t *testing.T) {
	st, fc := protectedProject()
	rec := st.sessions["mer-2"]
	rec.IsTerminated = true
	st.sessions["mer-2"] = rec
	svc := &Service{manager: fc, store: st}

	if err := svc.TeardownProject(context.Background(), "mer"); err != nil {
		t.Fatalf("TeardownProject: %v", err)
	}
	if len(fc.killed) != 2 {
		t.Fatalf("killed = %v, want the two live sessions", fc.killed)
	}
}

func TestAutoResumeAuthorityErrorsMapToTheRightAPIErrors(t *testing.T) {
	var apiErr *apierr.Error
	err := mapSessionError(fmt.Errorf("set auto-resume x: %w", sessionmanager.ErrAutoResumeAuthorityUnavailable))
	if !errors.As(err, &apiErr) || apiErr.Kind != apierr.KindUnavailable || apiErr.Code != "AUTO_RESUME_AUTHORITY_UNAVAILABLE" {
		t.Fatalf("authority unavailable = %v, want a 503-class AUTO_RESUME_AUTHORITY_UNAVAILABLE", err)
	}
	err = mapSessionError(fmt.Errorf("set auto-resume x: %w", sessionmanager.ErrAutoResumeNotPermitted))
	if !errors.As(err, &apiErr) || apiErr.Kind != apierr.KindForbidden || !strings.Contains(apiErr.Message, "--human") {
		t.Fatalf("not permitted = %v, want a 403 whose message tells the human about --human", err)
	}
}
