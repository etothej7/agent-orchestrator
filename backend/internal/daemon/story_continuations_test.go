package daemon

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/cdc"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	storysvc "github.com/aoagents/agent-orchestrator/backend/internal/service/story"
)

type continuationSweepProbe struct {
	storysvc.Store
	called chan struct{}
}

func (s *continuationSweepProbe) ListStories(context.Context) ([]domain.Story, error) {
	select {
	case s.called <- struct{}{}:
	default:
	}
	return nil, nil
}
func TestStoryContinuationsWaitForRecoveryAndUnsubscribeOnShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	recovered := make(chan struct{})
	events := cdc.NewBroadcaster()
	probe := &continuationSweepProbe{called: make(chan struct{}, 4)}
	runner := &storysvc.Continuations{Stories: &storysvc.Service{Store: probe}}
	done := startStoryContinuations(ctx, recovered, events, runner, slog.New(slog.DiscardHandler))
	select {
	case <-probe.called:
		t.Fatal("dispatch ran before provider recovery")
	case <-time.After(30 * time.Millisecond):
	}
	close(recovered)
	select {
	case <-probe.called:
	case <-time.After(time.Second):
		t.Fatal("startup did not reconcile")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("continuation loop did not stop")
	}
	if events.SubscriberCount() != 0 {
		t.Fatal("continuation subscription leaked")
	}
}
