package sessionmanager

import (
	"context"
	"testing"
	"time"

	claudeagent "github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/claudecode"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	browsersvc "github.com/aoagents/agent-orchestrator/backend/internal/service/browser"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// A session boot auto-resume parked wakes on the saved provider conversation or not at
// all; an ordinarily hibernated one keeps the existing wake (which may start fresh in
// place for a conversation AO can prove never persisted).
func TestWakeOfAnAutoParkedSessionNeverStartsFresh(t *testing.T) {
	const reserved = "92fadfcb-3d31-4dd6-93c1-097bf823cc19"
	for _, tc := range []struct {
		name       string
		autoParked bool
		wantFresh  bool
	}{
		{"ordinary hibernation", false, true},
		{"parked by auto-resume", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
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
				Activity:      domain.Activity{State: domain.ActivityIdle, LastActivityAt: now},
				FirstSignalAt: now, CreatedAt: now, UpdatedAt: now,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.CreateConversation(ctx, "conversation-1", domain.ConversationScopeSession, "proj", created.ID, now); err != nil {
				t.Fatal(err)
			}
			if applied, err := store.SetSessionHibernated(ctx, created.ID, created.Revision, &now); err != nil || !applied {
				t.Fatalf("hibernate = %v, %v", applied, err)
			}
			if tc.autoParked {
				intent := domain.ResumeIntent{SessionID: created.ID, BootID: "boot-1", CapturedAt: now, Class: domain.ResumeIdle, State: domain.ResumePending, UpdatedAt: now}
				stored, _, err := store.CaptureResumeIntent(ctx, intent)
				if err != nil {
					t.Fatal(err)
				}
				stored.State = domain.ResumeHibernated
				if ok, err := store.SaveResumeIntent(ctx, stored); err != nil || !ok {
					t.Fatalf("save intent = %v, %v", ok, err)
				}
			}
			launcher := &recordingLauncher{}
			manager := New(Deps{
				Runtime: &fakeRuntime{}, Agents: singleAgent{agent: claudeagent.New()}, Workspace: &fakeWorkspace{},
				Store: store, Messenger: &fakeMessenger{}, Chat: launcher,
				Lifecycle: &sqliteTransitionLifecycle{store: store},
				LookPath:  func(string) (string, error) { return "/bin/true", nil },
			})
			manager.browserCapabilities = &recordingBrowserAuthority{authority: browsersvc.NewAuthority()}

			_ = manager.WakeHibernatedChat(ctx, created.ID)

			if len(launcher.started) != 1 {
				t.Fatalf("started %d controllers, want 1", len(launcher.started))
			}
			if got := launcher.started[0].FreshIfProviderConversationMissing; got != tc.wantFresh {
				t.Fatalf("fresh-if-missing = %v, want %v", got, tc.wantFresh)
			}
			if tc.autoParked {
				if _, found, _ := store.GetResumeIntent(ctx, created.ID); found {
					t.Fatal("the parked intent survived the wake")
				}
			}
		})
	}
}
