package chat

import (
	"github.com/aoagents/agent-orchestrator/backend/internal/identity"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// sessionIDEnv names the variable that marks an MCP server as AO's own (`ao mcp`): the one tool
// server that calls the daemon on the session's behalf.
const sessionIDEnv = "AO_SESSION_ID"

// applyLaunchProof gives AO's own MCP server the identity proof of the launch whose environment
// the daemon just prepared. The MCP server config is fixed before a launch (and retained for
// later restarts), but a proof is minted per launch, so it is applied here, from the launch
// environment, never stored in the retained config. Drivers read their MCP servers after they
// have asked for the launch environment, so the proof is in place when the provider starts the
// server.
//
// Only servers that carry AO_SESSION_ID (the ao server) receive it: a third-party tool server
// never sees the session's identity. The proof of an environment without one is cleared, so a
// relaunch that lost its proof cannot leave the previous one behind.
func applyLaunchProof(servers []ports.ChatMCPServerConfig, launchEnv map[string]string) {
	proof := launchEnv[identity.EnvProof]
	for _, server := range servers {
		if server.Env == nil {
			continue
		}
		if _, isAO := server.Env[sessionIDEnv]; !isAO {
			continue
		}
		if proof == "" {
			delete(server.Env, identity.EnvProof)
			continue
		}
		server.Env[identity.EnvProof] = proof
	}
}
