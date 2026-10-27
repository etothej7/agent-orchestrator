package daemon

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/autoresume"
	sessionmanager "github.com/aoagents/agent-orchestrator/backend/internal/session_manager"
)

// The daemon hands the real session manager to the resumer and arms capture on it
// through type assertions, so a manager that loses a method would silently turn
// auto-resume off. These fail to compile instead.
var (
	_ autoresume.Manager                                    = (*sessionmanager.Manager)(nil)
	_ interface{ SetBootWindow(domain.BootWindow) }         = (*sessionmanager.Manager)(nil)
	_ interface{ WaitStartupChecks(context.Context) error } = (*sessionmanager.Manager)(nil)
)

// A session manager that cannot resume (a test double) turns the pass off rather
// than failing boot.
func TestRunAutoResumeSkipsAManagerThatCannotResume(t *testing.T) {
	runAutoResume(context.Background(), "boot", sessionLifecycle(nil), nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
}
