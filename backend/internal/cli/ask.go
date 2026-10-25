package cli

import (
	"errors"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

type humanDecisionAPIRequest struct {
	StoryID    string `json:"storyId,omitempty"`
	ID         string `json:"id"`
	Purpose    string `json:"purpose"`
	Question   string `json:"question"`
	WhyBlocked string `json:"whyBlocked"`
}

func newAskCommand(c *commandContext) *cobra.Command {
	var req humanDecisionAPIRequest
	cmd := &cobra.Command{Use: "ask", Short: "Ask the human for a genuinely blocking scope decision", Args: noArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		session := strings.TrimSpace(os.Getenv("AO_SESSION_ID"))
		if session == "" || strings.TrimSpace(req.ID) == "" || strings.TrimSpace(req.Question) == "" || strings.TrimSpace(req.WhyBlocked) == "" {
			return usageError{errors.New("AO_SESSION_ID, --id, --question and --why-blocked are required; retain --id for retries")}
		}
		return c.postJSON(cmd.Context(), "sessions/"+url.PathEscape(session)+"/conversation/human-decisions", req, nil)
	}}
	f := cmd.Flags()
	f.StringVar(&req.StoryID, "story", "", "Story paused by this decision; required when autonomous work exists")
	f.StringVar(&req.ID, "id", "", "Stable question/decision handle")
	f.StringVar(&req.Purpose, "purpose", "scope_question", "scope_question, blocking_decision or completion_decision")
	f.StringVar(&req.Question, "question", "", "The specific decision only the human can make")
	f.StringVar(&req.WhyBlocked, "why-blocked", "", "Why authorized work cannot proceed without this answer")
	return cmd
}
