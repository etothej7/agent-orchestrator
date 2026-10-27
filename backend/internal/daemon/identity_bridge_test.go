package daemon

import (
	"context"
	"io"
	"log/slog"
	"runtime"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

type noSessions struct{}

func (noSessions) ListAllSessions(context.Context) ([]domain.SessionRecord, error) { return nil, nil }

func TestTheAncestryBridgeCanBeSwitchedOff(t *testing.T) {
	t.Setenv("AO_PROCESS_ANCESTRY", "off")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if got := newAncestryBridge(log, noSessions{}, nil, t.TempDir()); got != nil {
		t.Fatal("the bridge was built although AO_PROCESS_ANCESTRY=off")
	}
}

// On macOS the bridge builds (the kernel lookup passes its self-test); elsewhere it is simply
// absent and the daemon has no bridge.
func TestTheAncestryBridgeIsAvailableOnMacOSAndAbsentElsewhere(t *testing.T) {
	t.Setenv("AO_PROCESS_ANCESTRY", "")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	got := newAncestryBridge(log, noSessions{}, nil, t.TempDir())
	if runtime.GOOS == "darwin" && got == nil {
		t.Fatal("no bridge on macOS")
	}
	if runtime.GOOS != "darwin" && got != nil {
		t.Fatal("a bridge on a platform with no process lookup")
	}
}
