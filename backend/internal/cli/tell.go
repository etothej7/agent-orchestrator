package cli

import (
	"errors"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// tellPurposes maps the operator-facing kind to the durable notice purpose.
var tellPurposes = map[string]string{"result": "verified_result", "stopped": "stopped_work"}

func newTellCommand(c *commandContext) *cobra.Command {
	var req humanDecisionAPIRequest
	var kind string
	cmd := &cobra.Command{Use: "tell", Short: "Tell the human one verified result or that work has stopped (overall coordinator only)", Args: noArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		session := strings.TrimSpace(os.Getenv("AO_SESSION_ID"))
		purpose, ok := tellPurposes[kind]
		if session == "" || !ok || strings.TrimSpace(req.ID) == "" || strings.TrimSpace(req.Question) == "" {
			return usageError{errors.New("AO_SESSION_ID, --id, --kind result|stopped and --message are required; retain --id for retries")}
		}
		req.Purpose = purpose
		return c.postJSON(cmd.Context(), "sessions/"+url.PathEscape(session)+"/conversation/human-decisions", req, nil)
	}}
	f := cmd.Flags()
	f.StringVar(&req.ID, "id", "", "Stable notice handle; a retry with the same id never notifies twice")
	f.StringVar(&kind, "kind", "", "result (a verified outcome the human should see) or stopped (authorized work has stopped and cannot continue)")
	f.StringVar(&req.Question, "message", "", "One concise message for the human")
	f.StringVar(&req.StoryID, "story", "", "Story this notice concerns (informational; never pauses it)")
	return cmd
}
