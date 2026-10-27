package daemon

import (
	"context"
	"log/slog"

	"github.com/aoagents/agent-orchestrator/backend/internal/service/autoresume"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

// runAutoResume runs one auto-resume pass for this boot. The resumer is built
// against narrow interfaces: the session manager is reached only through
// autoresume.Manager (a strict resume, a history probe and a park), so nothing here
// can kill a session, clean up a worktree, replace an orchestrator or tear a
// project down. A manager without those methods (a test double) disables the pass.
func runAutoResume(
	ctx context.Context, bootID string, sessMgr sessionLifecycle, chat *chatsvc.Service,
	store *sqlite.Store, notifier autoresume.HumanNotifier, log *slog.Logger,
) {
	manager, ok := sessMgr.(autoresume.Manager)
	if !ok {
		log.Info("auto-resume: session manager cannot resume, pass skipped")
		return
	}
	resumer := autoresume.New(bootID, manager, chat, store,
		autoresume.NoticeWriter{Store: store, Notify: notifier},
		autoresume.ContinueSender{Chat: chat}, log, autoresume.Config{})
	if err := resumer.Run(ctx); err != nil && ctx.Err() == nil {
		log.Error("auto-resume pass failed", "err", err)
	}
}
