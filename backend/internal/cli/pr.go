package cli

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

type mergePRResponse struct {
	OK       bool   `json:"ok"`
	PRNumber int    `json:"prNumber"`
	Method   string `json:"method"`
	// Request and Message are set instead when the project does not let agents merge: the daemon filed a
	// request for the person (202) rather than merging.
	Request struct {
		ID string `json:"id"`
	} `json:"request"`
	Message string `json:"message"`
}

// prSummary is what GET /api/v1/prs/<n> returns: the tracked pull request's URL and the head commit the daemon last saw.
type prSummary struct {
	PRNumber int    `json:"prNumber"`
	PRURL    string `json:"prUrl"`
	HeadSHA  string `json:"headSha"`
	State    string `json:"state"`
}

// mergePRRequest names exactly the pull request and head commit to merge; the daemon's route insists on both.
type mergePRRequest struct {
	PRURL           string `json:"prUrl"`
	ExpectedHeadSHA string `json:"expectedHeadSha"`
}

type resolveCommentsRequest struct {
	CommentIDs []string `json:"commentIds,omitempty"`
}

type resolveCommentsResponse struct {
	OK       bool `json:"ok"`
	Resolved int  `json:"resolved"`
}

func newPRCommand(ctx *commandContext) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pr",
		Short: "Manage pull requests",
	}
	cmd.AddCommand(newPRMergeCommand(ctx))
	cmd.AddCommand(newPRResolveCommentsCommand(ctx))
	return cmd
}

func newPRMergeCommand(ctx *commandContext) *cobra.Command {
	return &cobra.Command{
		Use:   "merge <pr-number>",
		Short: "Merge a pull request",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			prNumber, err := normalizePRNumber(args[0])
			if err != nil {
				return err
			}
			// The route merges only the exact pull request and head it is given, so ask the daemon which tracked
			// pull request this number is and what its head is now, and merge exactly that.
			var pr prSummary
			if err := ctx.getJSON(cmd.Context(), "prs/"+url.PathEscape(prNumber), &pr); err != nil {
				return err
			}
			if strings.TrimSpace(pr.PRURL) == "" || strings.TrimSpace(pr.HeadSHA) == "" {
				return fmt.Errorf("PR #%s has no known head commit yet; wait for AO to observe it and try again", prNumber)
			}
			var res mergePRResponse
			if err := ctx.postJSON(cmd.Context(), "prs/"+url.PathEscape(prNumber)+"/merge", mergePRRequest{PRURL: pr.PRURL, ExpectedHeadSHA: pr.HeadSHA}, &res); err != nil {
				return err
			}
			if res.Request.ID != "" {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s\n", strings.TrimSpace(res.Message))
				return err
			}
			if method := strings.TrimSpace(res.Method); method != "" {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "merged PR #%d using %s\n", res.PRNumber, method)
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "merged PR #%d\n", res.PRNumber)
			return err
		},
	}
}

func newPRResolveCommentsCommand(ctx *commandContext) *cobra.Command {
	return &cobra.Command{
		Use:   "resolve-comments <pr-number> [comment-id...]",
		Short: "Resolve review threads on a pull request (by comment or thread id)",
		Args:  usageArgs(cobra.MinimumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			prNumber, err := normalizePRNumber(args[0])
			if err != nil {
				return err
			}
			commentIDs := make([]string, 0, len(args)-1)
			for _, id := range args[1:] {
				id = strings.TrimSpace(id)
				if id == "" {
					return usageError{errors.New("comment id must not be blank")}
				}
				commentIDs = append(commentIDs, id)
			}
			var res resolveCommentsResponse
			if err := ctx.postJSON(
				cmd.Context(),
				"prs/"+url.PathEscape(prNumber)+"/resolve-comments",
				resolveCommentsRequest{CommentIDs: commentIDs},
				&res,
			); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "resolved %d review thread(s) on PR #%s\n", res.Resolved, prNumber)
			return err
		},
	}
}

func normalizePRNumber(raw string) (string, error) {
	raw = strings.TrimPrefix(strings.TrimSpace(raw), "#")
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return "", usageError{errors.New("PR number must be a positive integer")}
	}
	return strconv.Itoa(n), nil
}
