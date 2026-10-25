package cli

import (
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

type storyIntakeAPIRequest struct {
	ID              string `json:"id"`
	SourceMessageID string `json:"sourceMessageId"`
	SessionID       string `json:"sessionId"`
	Epic            struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	} `json:"epic"`
	Stories []struct {
		ID         string   `json:"id"`
		Title      string   `json:"title"`
		Scope      string   `json:"scope"`
		ProjectIDs []string `json:"projectIds"`
	} `json:"stories"`
}

func newIntakeCommand(c *commandContext) *cobra.Command {
	var plan string
	cmd := &cobra.Command{Use: "intake", Short: "Atomically adopt a human request into an epic and autonomous stories", Args: noArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		var req storyIntakeAPIRequest
		decoder := json.NewDecoder(strings.NewReader(plan))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&req); err != nil {
			return usageError{err}
		}
		req.SessionID = strings.TrimSpace(os.Getenv("AO_SESSION_ID"))
		if req.SessionID == "" || req.ID == "" || req.SourceMessageID == "" || len(req.Stories) == 0 {
			return usageError{errors.New("--plan must contain id, sourceMessageId, epic and stories; AO_SESSION_ID must select the coordinator")}
		}
		var result json.RawMessage
		if err := c.postJSON(cmd.Context(), "workspace/intake", req, &result); err != nil {
			return err
		}
		return writeJSON(cmd.OutOrStdout(), result)
	}}
	cmd.Flags().StringVar(&plan, "plan", "", "JSON {id,sourceMessageId,epic:{id,title},stories:[{id,title,scope,projectIds}]}; retain all ids on retry")
	cmd.AddCommand(&cobra.Command{Use: "context", Short: "Read the coordinator human conversation with durable message ids", Args: noArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		session := strings.TrimSpace(os.Getenv("AO_SESSION_ID"))
		if session == "" {
			return usageError{errors.New("AO_SESSION_ID is required")}
		}
		var result json.RawMessage
		if err := c.getJSON(cmd.Context(), "sessions/"+url.PathEscape(session)+"/conversation", &result); err != nil {
			return err
		}
		return writeJSON(cmd.OutOrStdout(), result)
	}})
	return cmd
}
