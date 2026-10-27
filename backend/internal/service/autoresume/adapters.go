package autoresume

import (
	"context"
	"crypto/sha256"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// ContinueText is the one turn a resumed session gets. It carries no copy of the
// original prompt: the provider's saved conversation already has it, and work that
// already ran tools must never run twice.
const ContinueText = "AO restarted. Continue where you left off. Check your last state before repeating any action (edits, commits, messages)."

// ChatSender is the chat service's keyed send.
type ChatSender interface {
	Send(ctx context.Context, id domain.SessionID, msg ports.ChatUserMessage) (domain.ConversationTurn, error)
}

// ContinueSender sends the continue turn as hidden automation. Its client message
// id is stable per session and boot, so a repeated sweep, a second boot of the same
// intent or a replay returns the recorded turn instead of sending another.
type ContinueSender struct{ Chat ChatSender }

// SendContinue implements Continuer.
func (c ContinueSender) SendContinue(ctx context.Context, id domain.SessionID, bootID string) error {
	_, err := c.Chat.Send(ctx, id, ports.ChatUserMessage{
		Text:            ContinueText,
		ClientMessageID: ContinueMessageID(id, bootID),
		Origin:          domain.MessageOriginAutomation,
	})
	return err
}

// ContinueMessageID is the idempotency key of a session's continue turn for a boot.
func ContinueMessageID(id domain.SessionID, bootID string) string {
	return "ao-autoresume-continue:" + string(id) + ":" + bootID
}

// NoticeStore is what the notice writer reads and writes.
type NoticeStore interface {
	GetCoordination(ctx context.Context) (domain.CoordinationConfig, error)
	ListAllSessions(ctx context.Context) ([]domain.SessionRecord, error)
	GetSession(ctx context.Context, id domain.SessionID) (domain.SessionRecord, bool, error)
	ConversationForSession(ctx context.Context, id domain.SessionID) (domain.ConversationRecord, error)
	RecordCoordinatorHumanMessage(ctx context.Context, msg domain.ConversationMessage, purpose, storyID string, now time.Time) (string, error)
}

// HumanNotifier raises the human-facing notification.
type HumanNotifier interface {
	Notify(ctx context.Context, intent ports.NotificationIntent) error
}

// NoticeWriter tells the human, through the coordinator conversation, that a session
// could not come back. It is a bounded `stopped_work` notice, not a status channel:
// one per session and boot, deduplicated and rate limited by the store, with a fixed
// reason phrase and never raw error text.
type NoticeWriter struct {
	Store  NoticeStore
	Notify HumanNotifier
	Now    func() time.Time
}

// NotifyResumeFailure implements Notifier.
func (w NoticeWriter) NotifyResumeFailure(ctx context.Context, id domain.SessionID, reason, bootID string) error {
	cfg, err := w.Store.GetCoordination(ctx)
	if err != nil || cfg.CoordinatorProjectID == "" {
		return err // nobody to tell without a coordinator project
	}
	victim, found, err := w.Store.GetSession(ctx, id)
	if err != nil || !found {
		return err
	}
	coordinator, ok, err := w.coordinator(ctx, cfg.CoordinatorProjectID)
	if err != nil || !ok {
		return err
	}
	conversation, err := w.Store.ConversationForSession(ctx, coordinator.ID)
	if err != nil {
		return err
	}
	name := victim.DisplayName
	if strings.TrimSpace(name) == "" {
		name = string(victim.ID)
	}
	text := fmt.Sprintf("Session %s couldn't resume after the restart: %s. It's stopped until you resume it.", name, reason)
	now := w.now()
	messageID, err := w.Store.RecordCoordinatorHumanMessage(ctx, domain.ConversationMessage{
		ID:             fmt.Sprintf("resume-failed-%x", sha256.Sum256([]byte(string(id)+"\x00"+bootID))),
		ConversationID: conversation.ID, SenderDisplayName: "AO", Text: text,
	}, "stopped_work", "", now)
	if err != nil || w.Notify == nil {
		return err
	}
	// Keyed by the message, so a retry after a lost response completes a missing
	// notification without raising a second.
	return w.Notify.Notify(ctx, ports.NotificationIntent{
		Type: domain.NotificationCoordinatorMessage, SessionID: coordinator.ID, ProjectID: coordinator.ProjectID, CreatedAt: now,
		SourceKey: "coordinator-message:" + messageID, SessionDisplayName: coordinator.DisplayName,
		Title: "Work has stopped and needs you", Body: text, Purpose: "stopped_work",
	})
}

// coordinator is the newest live orchestrator of the coordinator project. An exited
// one still owns the conversation, so the notice lands where the human looks.
func (w NoticeWriter) coordinator(ctx context.Context, project domain.ProjectID) (domain.SessionRecord, bool, error) {
	sessions, err := w.Store.ListAllSessions(ctx)
	if err != nil {
		return domain.SessionRecord{}, false, err
	}
	var candidates []domain.SessionRecord
	for _, rec := range sessions {
		if rec.Kind == domain.KindOrchestrator && rec.ProjectID == project && !rec.IsTerminated {
			candidates = append(candidates, rec)
		}
	}
	if len(candidates) == 0 {
		return domain.SessionRecord{}, false, nil
	}
	slices.SortFunc(candidates, func(a, b domain.SessionRecord) int { return b.CreatedAt.Compare(a.CreatedAt) })
	return candidates[0], true, nil
}

func (w NoticeWriter) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now().UTC()
}
