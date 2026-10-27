package autoresume

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type recordingChat struct{ sent []ports.ChatUserMessage }

func (c *recordingChat) Send(_ context.Context, _ domain.SessionID, msg ports.ChatUserMessage) (domain.ConversationTurn, error) {
	c.sent = append(c.sent, msg)
	return domain.ConversationTurn{}, nil
}

func TestContinueIsOneHiddenAutomationTurnKeyedBySessionAndBoot(t *testing.T) {
	chat := &recordingChat{}
	sender := ContinueSender{Chat: chat}
	for range 2 {
		if err := sender.SendContinue(context.Background(), "w-1", "boot-7"); err != nil {
			t.Fatal(err)
		}
	}
	if len(chat.sent) != 2 || chat.sent[0].ClientMessageID != chat.sent[1].ClientMessageID {
		t.Fatalf("the same session and boot must reuse one idempotency key: %+v", chat.sent)
	}
	got := chat.sent[0]
	if got.ClientMessageID != "ao-autoresume-continue:w-1:boot-7" || got.Origin != domain.MessageOriginAutomation || got.Text != ContinueText {
		t.Fatalf("continue turn = %+v", got)
	}
	if got.SenderSessionID != "" {
		t.Fatal("the continue turn must not claim a sender session")
	}
	if strings.Contains(strings.ToLower(got.Text), "original") || !strings.Contains(got.Text, "before repeating any action") {
		t.Fatalf("the continue turn must not replay the task and must warn against repeating actions: %q", got.Text)
	}
	other := ContinueMessageID("w-1", "boot-8")
	if other == got.ClientMessageID {
		t.Fatal("a different boot must get a different key")
	}
}

type noticeStore struct {
	cfg      domain.CoordinationConfig
	sessions []domain.SessionRecord
	messages []domain.ConversationMessage
	purposes []string
}

func (s *noticeStore) GetCoordination(context.Context) (domain.CoordinationConfig, error) {
	return s.cfg, nil
}
func (s *noticeStore) ListAllSessions(context.Context) ([]domain.SessionRecord, error) {
	return s.sessions, nil
}
func (s *noticeStore) GetSession(_ context.Context, id domain.SessionID) (domain.SessionRecord, bool, error) {
	for _, rec := range s.sessions {
		if rec.ID == id {
			return rec, true, nil
		}
	}
	return domain.SessionRecord{}, false, nil
}
func (s *noticeStore) ConversationForSession(_ context.Context, id domain.SessionID) (domain.ConversationRecord, error) {
	return domain.ConversationRecord{ID: "conv-of-" + string(id)}, nil
}
func (s *noticeStore) RecordCoordinatorHumanMessage(_ context.Context, msg domain.ConversationMessage, purpose, _ string, _ time.Time) (string, error) {
	s.messages = append(s.messages, msg)
	s.purposes = append(s.purposes, purpose)
	return msg.ID, nil
}

type recordingNotifier struct{ intents []ports.NotificationIntent }

func (n *recordingNotifier) Notify(_ context.Context, intent ports.NotificationIntent) error {
	n.intents = append(n.intents, intent)
	return nil
}

func TestResumeFailureNoticeIsAFixedPhraseStoppedWorkNoticeInTheCoordinatorConversation(t *testing.T) {
	store := &noticeStore{
		cfg: domain.CoordinationConfig{CoordinatorProjectID: "coord"},
		sessions: []domain.SessionRecord{
			{ID: "coord-old", Kind: domain.KindOrchestrator, ProjectID: "coord", IsTerminated: true, CreatedAt: t0},
			{ID: "coord-1", Kind: domain.KindOrchestrator, ProjectID: "coord", CreatedAt: t0.Add(time.Hour), DisplayName: "Coordinator"},
			{ID: "w-1", Kind: domain.KindWorker, ProjectID: "repo", DisplayName: "Fix the build"},
		},
	}
	notifier := &recordingNotifier{}
	writer := NoticeWriter{Store: store, Notify: notifier, Now: func() time.Time { return t0 }}

	for range 2 { // a retry after a lost response
		if err := writer.NotifyResumeFailure(context.Background(), "w-1", "sign-in required", "boot-7"); err != nil {
			t.Fatal(err)
		}
	}
	if len(store.messages) != 2 || store.messages[0].ID != store.messages[1].ID {
		t.Fatalf("the message id must be deterministic per session and boot: %+v", store.messages)
	}
	msg := store.messages[0]
	if msg.ConversationID != "conv-of-coord-1" || msg.SenderDisplayName != "AO" || msg.SenderSessionID != "" ||
		store.purposes[0] != "stopped_work" {
		t.Fatalf("notice = %+v purpose=%q, want a system-authored stopped_work message in the live coordinator's conversation", msg, store.purposes[0])
	}
	if want := "Session Fix the build couldn't resume after the restart: sign-in required. It's stopped until you resume it."; msg.Text != want {
		t.Fatalf("text = %q, want %q", msg.Text, want)
	}
	if len(notifier.intents) != 2 || notifier.intents[0].SourceKey != "coordinator-message:"+msg.ID ||
		notifier.intents[0].Purpose != "stopped_work" || notifier.intents[0].SessionID != "coord-1" ||
		notifier.intents[0].Type != domain.NotificationCoordinatorMessage {
		t.Fatalf("notification = %+v, want one keyed by the message", notifier.intents)
	}

	// Another boot is another notice; another session is another notice.
	if err := writer.NotifyResumeFailure(context.Background(), "w-1", "sign-in required", "boot-8"); err != nil {
		t.Fatal(err)
	}
	if store.messages[2].ID == msg.ID {
		t.Fatal("a later boot reused the message id")
	}
}

func TestResumeFailureNoticeWithoutACoordinatorWritesNothing(t *testing.T) {
	store := &noticeStore{sessions: []domain.SessionRecord{{ID: "w-1", Kind: domain.KindWorker, ProjectID: "repo"}}}
	notifier := &recordingNotifier{}
	if err := (NoticeWriter{Store: store, Notify: notifier}).NotifyResumeFailure(context.Background(), "w-1", "x", "b"); err != nil {
		t.Fatal(err)
	}
	if len(store.messages) != 0 || len(notifier.intents) != 0 {
		t.Fatalf("a notice was written with no coordinator project: %+v %+v", store.messages, notifier.intents)
	}
	store.cfg = domain.CoordinationConfig{CoordinatorProjectID: "coord"}
	if err := (NoticeWriter{Store: store, Notify: notifier}).NotifyResumeFailure(context.Background(), "w-1", "x", "b"); err != nil {
		t.Fatal(err)
	}
	if len(store.messages) != 0 {
		t.Fatal("a notice was written with no coordinator session to own the conversation")
	}
}

// The identity relaunch's give-up notice is the same stopped_work notice in the coordinator's
// conversation, worded for what happened, and independent of the restart notice for the same
// session and boot.
func TestRelaunchFailureNoticeIsAStoppedWorkNoticeWithItsOwnWording(t *testing.T) {
	store := &noticeStore{
		cfg: domain.CoordinationConfig{CoordinatorProjectID: "coord"},
		sessions: []domain.SessionRecord{
			{ID: "coord-1", Kind: domain.KindOrchestrator, ProjectID: "coord", CreatedAt: t0},
			{ID: "w-1", Kind: domain.KindWorker, ProjectID: "repo", DisplayName: "Fix the build"},
		},
	}
	notifier := &recordingNotifier{}
	writer := NoticeWriter{Store: store, Notify: notifier, Now: func() time.Time { return t0 }}
	if err := writer.NotifyRelaunchFailure(context.Background(), "w-1", "it could not be started again", "reproof-1"); err != nil {
		t.Fatal(err)
	}
	if err := writer.NotifyResumeFailure(context.Background(), "w-1", "x", "reproof-1"); err != nil {
		t.Fatal(err)
	}
	if len(store.messages) != 2 || store.messages[0].ID == store.messages[1].ID {
		t.Fatalf("messages = %+v; the relaunch and restart notices must not share an id", store.messages)
	}
	want := "Session Fix the build was stopped to give it an identity proof and couldn't be started again: it could not be started again. It's stopped until you resume it."
	if store.messages[0].Text != want || store.purposes[0] != "stopped_work" {
		t.Fatalf("notice = %q purpose %q, want %q as stopped_work", store.messages[0].Text, store.purposes[0], want)
	}
}
