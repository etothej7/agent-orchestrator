package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// A human-only action cannot be done from a terminal or by an agent: they file a request, the daemon
// renders what it says, and the person approves or denies it in the AO app. These commands only file.

type humanRequestAPIRequest struct {
	Kind      string         `json:"kind"`
	SessionID string         `json:"sessionId"`
	Payload   map[string]any `json:"payload,omitempty"`
	// RequesterSessionID is attribution only: the card says who the caller claims to be.
	RequesterSessionID string `json:"requesterSessionId,omitempty"`
}

type humanRequestAPIResponse struct {
	Request struct {
		ID      string `json:"id"`
		State   string `json:"state"`
		Summary string `json:"summary"`
	} `json:"request"`
	Message string `json:"message"`
}

// fileHumanRequest asks the person to confirm a human-only action and prints what the daemon rendered.
func (c *commandContext) fileHumanRequest(cmd *cobra.Command, kind, sessionID string, payload map[string]any) error {
	req := humanRequestAPIRequest{Kind: kind, SessionID: sessionID, Payload: payload, RequesterSessionID: strings.TrimSpace(os.Getenv("AO_SESSION_ID"))}
	var res humanRequestAPIResponse
	if err := c.postJSON(cmd.Context(), "human-requests", req, &res); err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if _, err := fmt.Fprintf(out, "filed request %s (%s): %s\n", res.Request.ID, res.Request.State, res.Request.Summary); err != nil {
		return err
	}
	if res.Message != "" {
		_, err := fmt.Fprintln(out, res.Message)
		return err
	}
	return nil
}

func newSessionSetModelCommand(ctx *commandContext) *cobra.Command {
	var model, effort string
	var fast, plan bool
	cmd := &cobra.Command{
		Use:   "set-model <id>",
		Short: "Ask the person to change a chat session's model or reasoning effort",
		Long: "Changing what a conversation runs on is the person's decision. This files a request that the person\n" +
			"approves with one click in the AO app; nothing changes until they do. You are told how it ends.\n" +
			"Name at least one of --model, --effort, --fast, --plan. A permission mode cannot be requested here.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := normalizeSessionID(args[0])
			if err != nil {
				return err
			}
			payload := map[string]any{}
			flags := cmd.Flags()
			if flags.Changed("model") {
				payload["model"] = model
			}
			if flags.Changed("effort") {
				payload["reasoningEffort"] = effort
			}
			if flags.Changed("fast") {
				payload["fastMode"] = fast
			}
			if flags.Changed("plan") {
				payload["planMode"] = plan
			}
			if len(payload) == 0 {
				return usageError{errors.New("name at least one of --model, --effort, --fast, --plan")}
			}
			return ctx.fileHumanRequest(cmd, "conversation_settings", id, payload)
		},
	}
	f := cmd.Flags()
	f.StringVar(&model, "model", "", "Model id the conversation should use")
	f.StringVar(&effort, "effort", "", "Reasoning effort to use")
	f.BoolVar(&fast, "fast", false, "Fast mode on (--fast=false for off)")
	f.BoolVar(&plan, "plan", false, "Plan mode on (--plan=false for off)")
	return cmd
}

func newSessionSwitchAgentCommand(ctx *commandContext) *cobra.Command {
	var agent, model, effort string
	var fast bool
	cmd := &cobra.Command{
		Use:   "switch-agent <id>",
		Short: "Ask the person to move a chat session to another agent",
		Long: "Moving a conversation to another agent is the person's decision. This files a request that the person\n" +
			"approves with one click in the AO app; nothing changes until they do. You are told how it ends.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := normalizeSessionID(args[0])
			if err != nil {
				return err
			}
			if strings.TrimSpace(agent) == "" {
				return usageError{errors.New("--agent is required")}
			}
			payload := map[string]any{"targetHarness": agent}
			flags := cmd.Flags()
			if flags.Changed("model") {
				payload["model"] = model
			}
			if flags.Changed("effort") {
				payload["effort"] = effort
			}
			if flags.Changed("fast") {
				payload["fastMode"] = fast
			}
			return ctx.fileHumanRequest(cmd, "provider_switch", id, payload)
		},
	}
	f := cmd.Flags()
	f.StringVar(&agent, "agent", "", "Agent to move the session to, for example codex")
	f.StringVar(&model, "model", "", "Model the target starts on")
	f.StringVar(&effort, "effort", "", "Reasoning effort the target starts on")
	f.BoolVar(&fast, "fast", false, "Start the target on its faster tier")
	return cmd
}
