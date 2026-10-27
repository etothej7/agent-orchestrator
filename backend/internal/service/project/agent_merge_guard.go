package project

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/identity"
)

// codeHumanRequired is the code every refusal to a non-person caller carries (the app shows its own notice for it).
const codeHumanRequired = "HUMAN_CAPABILITY_REQUIRED"

// confineAgentMerge makes agentMayMerge the person's setting alone. A project's config is written wholesale by
// several routes (add, settings, `ao project config set`), and an agent that could write agentMayMerge: true
// could let itself merge. So unless the caller holds the human capability:
//
//   - turning it on is refused, and
//   - the stored value is kept, whatever the config says (an agent's rewrite of the rest of the config neither
//     enables nor silently revokes what the person chose).
//
// stored is the project's current value (false for a project being added). It edits cfg in place.
func confineAgentMerge(ctx context.Context, stored bool, cfg *domain.ProjectConfig) error {
	if identity.FromContext(ctx).IsHuman() {
		return nil
	}
	if cfg.AgentMayMerge && !stored {
		return apierr.Forbidden(codeHumanRequired, "Only the person using the desktop app can let agents merge pull requests (agentMayMerge). Ask them to turn it on in the project's settings.")
	}
	cfg.AgentMayMerge = stored
	return nil
}
