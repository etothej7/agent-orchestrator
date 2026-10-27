package chat_test

import (
	"context"
	"log/slog"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/identity"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
)

// A chat session's identity reaches the tools it runs through AO's own MCP server. The server
// config is fixed before the launch, so the launch's proof is applied when the daemon prepares the
// launch environment, which drivers do before they read their MCP servers.
func TestChatLaunchGivesTheProofToTheAOMCPServerOnly(t *testing.T) {
	st := openStore(t)
	var started ports.ChatStartConfig
	svc := chatsvc.New(chatsvc.Options{
		Store: st, Reader: fullSnapshotReader(st), Sessions: st,
		Drivers: fakeRegistry{driver: fakeDriver{conv: newFakeConversation(), startCfg: &started}},
		Log:     slog.New(slog.DiscardHandler), NewID: func() string { return "launch-proof-id" },
	})
	proof, err := identity.NewProof()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Start(context.Background(), chatsvc.StartConfig{
		SessionID: testSession, ProjectID: testProject, Harness: domain.HarnessCodex, WorkspacePath: t.TempDir(),
		MCPServers: []ports.ChatMCPServerConfig{
			{Name: "ao", Type: "stdio", Command: "/opt/ao", Args: []string{"mcp"}, Env: map[string]string{"AO_SESSION_ID": string(testSession), "AO_DATA_DIR": "/data"}},
			{Name: "third-party", Type: "stdio", Command: "/opt/other", Env: map[string]string{"TOKEN": "x"}},
		},
		PrepareControllerEnv: func(context.Context, domain.SessionControllerOwner) (map[string]string, error) {
			return map[string]string{identity.EnvProof: proof, "AO_SESSION_ID": string(testSession)}, nil
		},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.StopAll(context.Background())

	var ao, other *ports.ChatMCPServerConfig
	for i := range started.MCPServers {
		switch started.MCPServers[i].Name {
		case "ao":
			ao = &started.MCPServers[i]
		case "third-party":
			other = &started.MCPServers[i]
		}
	}
	if ao == nil || other == nil {
		t.Fatalf("driver saw MCP servers %+v", started.MCPServers)
	}
	if ao.Env[identity.EnvProof] != proof {
		t.Fatalf("the ao MCP server did not get the launch's proof: %v", ao.Env)
	}
	if _, leaked := other.Env[identity.EnvProof]; leaked {
		t.Fatal("a third-party MCP server received the session's identity proof")
	}
	if started.Env[identity.EnvProof] != proof {
		t.Fatalf("the provider process env lacks the proof: %v", started.Env)
	}
}

// A launch whose environment has no proof (identity not enabled, or lost) must not leave a
// previous launch's proof in the MCP server.
func TestChatLaunchWithoutAProofClearsTheMCPServersProof(t *testing.T) {
	st := openStore(t)
	var started ports.ChatStartConfig
	svc := chatsvc.New(chatsvc.Options{
		Store: st, Reader: fullSnapshotReader(st), Sessions: st,
		Drivers: fakeRegistry{driver: fakeDriver{conv: newFakeConversation(), startCfg: &started}},
		Log:     slog.New(slog.DiscardHandler), NewID: func() string { return "launch-no-proof-id" },
	})
	if _, err := svc.Start(context.Background(), chatsvc.StartConfig{
		SessionID: testSession, ProjectID: testProject, Harness: domain.HarnessCodex, WorkspacePath: t.TempDir(),
		MCPServers: []ports.ChatMCPServerConfig{{Name: "ao", Type: "stdio", Command: "/opt/ao", Env: map[string]string{"AO_SESSION_ID": string(testSession), identity.EnvProof: "aop1.stale"}}},
		PrepareControllerEnv: func(context.Context, domain.SessionControllerOwner) (map[string]string, error) {
			return map[string]string{"AO_SESSION_ID": string(testSession)}, nil
		},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.StopAll(context.Background())
	if got := started.MCPServers[0].Env[identity.EnvProof]; got != "" {
		t.Fatalf("a stale proof survived a launch with no proof: %q", got)
	}
}
