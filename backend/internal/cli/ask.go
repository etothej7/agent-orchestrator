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
	cmd.AddCommand(newAskResolveCommand(c))
	f := cmd.Flags()
	f.StringVar(&req.StoryID, "story", "", "Story paused by this decision; required when autonomous work exists")
	f.StringVar(&req.ID, "id", "", "Stable question/decision handle")
	f.StringVar(&req.Purpose, "purpose", "scope_question", "scope_question, blocking_decision or completion_decision")
	f.StringVar(&req.Question, "question", "", "The specific decision only the human can make")
	f.StringVar(&req.WhyBlocked, "why-blocked", "", "Why authorized work cannot proceed without this answer")
	return cmd
}

type resolveHumanDecisionAPIRequest struct {
	ID   string `json:"id"`
	Note string `json:"note,omitempty"`
}

// newAskResolveCommand records that Ed answered one of this coordinator's
// questions. A question stays open until it is answered explicitly: Ed's Answer
// control, or this command after he answered in free text. An unrelated message
// never closes it.
func newAskResolveCommand(c *commandContext) *cobra.Command {
	var req resolveHumanDecisionAPIRequest
	cmd := &cobra.Command{Use: "resolve", Short: "Record that the human answered one of your questions", Args: noArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		session := strings.TrimSpace(os.Getenv("AO_SESSION_ID"))
		if session == "" || strings.TrimSpace(req.ID) == "" {
			return usageError{errors.New("AO_SESSION_ID and --id (the ask id you gave ao ask) are required")}
		}
		return c.postJSON(cmd.Context(), "sessions/"+url.PathEscape(session)+"/conversation/human-decisions/resolve", req, nil)
	}}
	f := cmd.Flags()
	f.StringVar(&req.ID, "id", "", "The ask id given to ao ask --id")
	f.StringVar(&req.Note, "note", "", "Optional short note, for example what the human answered")
	return cmd
}
