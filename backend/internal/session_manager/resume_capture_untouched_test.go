package sessionmanager

import (
	"context"
	"testing"
	"time"

	claudeagent "github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/claudecode"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	browsersvc "github.com/aoagents/agent-orchestrator/backend/internal/service/browser"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// An untouched chat (a reserved provider id the provider never persisted, no turns) has
// nothing to resume: capturing it would only produce a "can't be resumed safely" notice
// for an empty chat. A chat that did run a turn is captured as usual.
func TestCaptureSkipsAnUntouchedChatButNotOneThatRan(t *testing.T) {
	const reserved = "92fadfcb-3d31-4dd6-93c1-097bf823cc19"
	for _, tc := range []struct {
		name        string
		providerRan bool
		wantIntent  bool
	}{
		{"untouched chat", false, false},
		{"chat with a provider turn", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // no transcript exists for the reserved id
			ctx := context.Background()
			store := sqlitetest.MustOpenAt(t, t.TempDir())
			if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "proj", Path: t.TempDir()}); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			created, err := store.CreateSession(ctx, domain.SessionRecord{
				ProjectID: "proj", Kind: domain.KindWorker, Harness: domain.HarnessClaudeCode, Mode: domain.SessionModeChat,
				Metadata: domain.SessionMetadata{
					WorkspacePath: t.TempDir(), Branch: "ao/session",
					ProviderConversationID: reserved, ControllerGeneration: "chat-generation",
				},
				Activity:      domain.Activity{State: domain.ActivityActive, LastActivityAt: now},
				FirstSignalAt: now, CreatedAt: now, UpdatedAt: now,
			})
			if err != nil {
				t.Fatal(err)
			}
			conversation, err := store.CreateConversation(ctx, "conversation-1", domain.ConversationScopeSession, "proj", created.ID, now)
			if err != nil {
				t.Fatal(err)
			}
			if tc.providerRan {
				if err := store.AdoptProviderTurn(ctx, conversation.ID, created.ID, "chat-generation", "turn-1", "provider-turn-1", now); err != nil {
					t.Fatal(err)
				}
			}
			manager := New(Deps{
				Runtime: &fakeRuntime{}, Agents: singleAgent{agent: claudeagent.New()}, Workspace: &fakeWorkspace{},
				Store: store, Messenger: &fakeMessenger{}, Chat: &recordingLauncher{startErr: ports.ErrChatHostNotRunning},
				Lifecycle: &sqliteTransitionLifecycle{store: store},
				LookPath:  func(string) (string, error) { return "/bin/true", nil },
			})
			manager.browserCapabilities = &recordingBrowserAuthority{authority: browsersvc.NewAuthority()}
			manager.SetBootWindow(domain.NewBootWindow("boot-2", now.Add(time.Minute), nil, time.Time{}))

			rec, _, err := store.GetSession(ctx, created.ID)
			if err != nil {
				t.Fatal(err)
			}
			_ = manager.checkSessionHealth(ctx, rec)

			_, found, err := store.GetResumeIntent(ctx, created.ID)
			if err != nil || found != tc.wantIntent {
				t.Fatalf("intent captured = %v (err %v), want %v", found, err, tc.wantIntent)
			}
		})
	}
}
