package cli

import (
	"errors"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

type storyCommandAPIRequest struct {
	Outcome            string `json:"outcome"`
	ID                 string `json:"id"`
	StoryID            string `json:"storyId"`
	Revision           int64  `json:"revision"`
	Kind               string `json:"kind"`
	SenderSessionID    string `json:"senderSessionId"`
	RecipientSessionID string `json:"recipientSessionId"`
	Text               string `json:"text"`
	Reference          string `json:"reference"`
	Role               string `json:"role"`
}

func newStoryCommand(c *commandContext) *cobra.Command {
	var req storyCommandAPIRequest
	cmd := &cobra.Command{Use: "story", Short: "Record an explicit story exchange or decision", Args: noArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if req.ID == "" || req.StoryID == "" || req.Kind == "" {
			return usageError{errors.New("--story, --id and --kind are required; retain --id for retries")}
		}
		req.SenderSessionID = strings.TrimSpace(os.Getenv("AO_SESSION_ID"))
		var detail struct {
			Story struct {
				Revision int64 `json:"revision"`
			} `json:"story"`
		}
		if err := c.getJSON(cmd.Context(), "workspace/stories/"+url.PathEscape(req.StoryID), &detail); err != nil {
			return err
		}
		req.Revision = detail.Story.Revision
		return c.postJSON(cmd.Context(), "workspace/commands", req, nil)
	}}
	f := cmd.Flags()
	f.StringVar(&req.Outcome, "outcome", "", "Testing result: passed or failed")
	f.StringVar(&req.StoryID, "story", "", "Explicit story id")
	f.StringVar(&req.ID, "id", "", "Stable operation id; retain for retries")
	f.StringVar(&req.Kind, "kind", "", "assignment, message, handoff, blocked, unblocked, question, answer, implementation, testing, review_passed, review_failed, in_progress, in_review, ready_for_approval, done, installation, release")
	f.StringVar(&req.RecipientSessionID, "session", "", "Recipient session for assignment or delivery")
	f.StringVar(&req.Role, "role", "", "Assignment role: manager, worker, reviewer")
	f.StringVar(&req.Text, "text", "", "Public message or decision explanation")
	f.StringVar(&req.Reference, "reference", "", "Evidence reference or fixed revision")
	return cmd
}
