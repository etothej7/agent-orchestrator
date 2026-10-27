package chat_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

// An unattended Chat resume continues the saved provider conversation in
// ChatHistoryImport with no fresh start, so what it recovers depends only on what
// the driver can do after a reboot. One test per history mode a driver can offer:
// typed replay (ACP session/load, Codex thread/read), model context only (ACP
// session/resume), and nothing to resume.
func TestUnattendedChatResumeHistoryModes(t *testing.T) {
	const saved = "saved-provider-thread"
	resumeOnly := func(t *testing.T, conv ports.ChatConversation, resume func(ports.ChatResumeConfig) (ports.ChatConversation, error)) (
		*sqlite.Store, *chatsvc.Service, *ports.ChatStartConfig, *ports.ChatResumeConfig, error,
	) {
		t.Helper()
		st := openStore(t)
		var started ports.ChatStartConfig
		var resumed ports.ChatResumeConfig
		var nextID atomic.Int64
		svc := chatsvc.New(chatsvc.Options{
			Store: st, Sessions: st,
			Drivers: fakeRegistry{driver: fakeDriver{conv: conv, startCfg: &started, resumeCfg: &resumed, resume: resume}},
			Log:     slog.New(slog.DiscardHandler),
			NewID:   func() string { return fmt.Sprintf("history-mode-%d", nextID.Add(1)) },
		})
		t.Cleanup(func() { _ = svc.Stop(context.Background(), testSession) })
		_, err := svc.Start(context.Background(), chatsvc.StartConfig{
			SessionID: testSession, ProjectID: testProject, Harness: domain.HarnessClaudeCode,
			WorkspacePath: t.TempDir(), ProviderConversationID: saved, HistoryMode: ports.ChatHistoryImport,
		})
		return st, svc, &started, &resumed, err
	}

	t.Run("full typed replay is imported", func(t *testing.T) {
		conv := &nativeHistoryConversation{
			fakeConversation: newFakeConversation(),
			events: []ports.ChatEvent{
				{Kind: ports.ChatEventTurnStarted, ProviderEventID: "h-start", ProviderTurnID: "native-turn-1"},
				{Kind: ports.ChatEventUserMessageCompleted, ProviderEventID: "h-user", ProviderTurnID: "native-turn-1",
					ProviderItemID: "h-item-1", ClientMessageID: "native-client-1", Text: "Fix the build."},
				{Kind: ports.ChatEventMessageCompleted, ProviderEventID: "h-answer", ProviderTurnID: "native-turn-1",
					ProviderItemID: "h-item-2", Text: "Fixed."},
				{Kind: ports.ChatEventTurnCompleted, ProviderEventID: "h-done", ProviderTurnID: "native-turn-1",
					TurnState: domain.TurnStateRecovered},
			},
		}
		conv.providerConversationID = saved
		st, svc, started, resumed, err := resumeOnly(t, conv, nil)
		if err != nil {
			t.Fatalf("resume with typed replay: %v", err)
		}
		if resumed.ProviderConversationID != saved || resumed.FreshIfMissing || started.SessionID != "" {
			t.Fatalf("must resume the saved conversation, never fresh: resumed=%+v started=%+v", resumed, started)
		}
		if conv.historyReads() != 1 {
			t.Fatalf("history reads = %d, want the replay read once", conv.historyReads())
		}
		ctrl, err := svc.Controller(testSession)
		if err != nil {
			t.Fatalf("Controller: %v", err)
		}
		snapshot, err := st.LoadConversationSnapshot(context.Background(), ctrl.ConversationID())
		if err != nil {
			t.Fatalf("snapshot: %v", err)
		}
		if len(snapshot.Messages) != 2 || snapshot.Messages[0].Text != "Fix the build." || snapshot.Messages[1].Text != "Fixed." {
			t.Fatalf("typed replay was not imported: %#v", snapshot.Messages)
		}
	})

	t.Run("model context only still resumes the same conversation", func(t *testing.T) {
		conv := &nativeHistoryConversation{fakeConversation: newFakeConversation(), err: ports.ErrChatHistoryUnavailable}
		conv.providerConversationID = saved
		st, svc, started, resumed, err := resumeOnly(t, conv, nil)
		if err != nil {
			t.Fatalf("a resume-only provider must still resume: %v", err)
		}
		if resumed.ProviderConversationID != saved || resumed.FreshIfMissing || started.SessionID != "" {
			t.Fatalf("must resume the saved conversation, never fresh: resumed=%+v started=%+v", resumed, started)
		}
		ctrl, err := svc.Controller(testSession)
		if err != nil {
			t.Fatalf("Controller: %v", err)
		}
		if got := ctrl.ProviderConversationID(); got != saved {
			t.Fatalf("controller is on %q, want the saved provider conversation %q", got, saved)
		}
		snapshot, err := st.LoadConversationSnapshot(context.Background(), ctrl.ConversationID())
		if err != nil {
			t.Fatalf("snapshot: %v", err)
		}
		if len(snapshot.Messages) != 0 {
			t.Fatalf("a provider with no typed replay must import nothing: %#v", snapshot.Messages)
		}
	})

	t.Run("a provider with no saved session fails and never starts fresh", func(t *testing.T) {
		conv := newFakeConversation()
		_, _, started, resumed, err := resumeOnly(t, conv, func(ports.ChatResumeConfig) (ports.ChatConversation, error) {
			return nil, fmt.Errorf("session not found: %w", ports.ErrChatResumeFailed)
		})
		if !errors.Is(err, ports.ErrChatResumeFailed) {
			t.Fatalf("Start error = %v, want ErrChatResumeFailed", err)
		}
		if resumed.FreshIfMissing || started.SessionID != "" {
			t.Fatalf("a missing provider session must not start a fresh conversation: resumed=%+v started=%+v", resumed, started)
		}
	})
}
