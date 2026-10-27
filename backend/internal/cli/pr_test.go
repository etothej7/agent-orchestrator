package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const prHeadSHA = "0123456789abcdef0123456789abcdef01234567"

type prMergeCalls struct {
	getPath   string
	mergePath string
	mergeBody string
}

// prMergeServer answers GET /api/v1/prs/<n> with the tracked PR and POST .../merge with the given response.
func prMergeServer(t *testing.T, summary string, mergeStatus int, mergeBody string) (*httptest.Server, *prMergeCalls) {
	t.Helper()
	calls := &prMergeCalls{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/prs/") && !strings.HasSuffix(r.URL.Path, "/merge"):
			calls.getPath = r.URL.Path
			_, _ = io.WriteString(w, summary)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/merge"):
			raw, _ := io.ReadAll(r.Body)
			calls.mergePath, calls.mergeBody = r.URL.Path, string(raw)
			w.WriteHeader(mergeStatus)
			_, _ = io.WriteString(w, mergeBody)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, calls
}

const trackedPR = `{"prNumber":42,"prUrl":"https://github.com/acme/repo/pull/42","headSha":"` + prHeadSHA + `","state":"open","sessionId":"repo-1"}`

func TestPRMergeNamesTheTrackedPRAndItsHeadToTheDaemon(t *testing.T) {
	cfg := setConfigEnv(t)
	srv, calls := prMergeServer(t, trackedPR, http.StatusOK, `{"ok":true,"prNumber":42,"method":"squash"}`)
	writeRunFileFor(t, cfg, srv)

	out, errOut, err := executeCLI(t, aliveDeps(), "pr", "merge", "#42")
	if err != nil {
		t.Fatalf("unexpected error: %v\nstderr=%s", err, errOut)
	}
	if calls.getPath != "/api/v1/prs/42" || calls.mergePath != "/api/v1/prs/42/merge" {
		t.Fatalf("calls = %+v", calls)
	}
	var body map[string]string
	if err := json.Unmarshal([]byte(calls.mergeBody), &body); err != nil {
		t.Fatal(err)
	}
	if body["prUrl"] != "https://github.com/acme/repo/pull/42" || body["expectedHeadSha"] != prHeadSHA || len(body) != 2 {
		t.Fatalf("merge body = %s; it must name exactly the tracked PR and its current head", calls.mergeBody)
	}
	if !strings.Contains(out, "merged PR #42 using squash") {
		t.Fatalf("stdout = %q", out)
	}
}

func TestPRMergeSaysSoWhenTheDaemonFiledARequestInsteadOfMerging(t *testing.T) {
	cfg := setConfigEnv(t)
	srv, _ := prMergeServer(t, trackedPR, http.StatusAccepted, `{"request":{"id":"hr-9"},"message":"Filed request hr-9 for the person to approve in the AO app. Do not retry it."}`)
	writeRunFileFor(t, cfg, srv)

	out, errOut, err := executeCLI(t, aliveDeps(), "pr", "merge", "42")
	if err != nil {
		t.Fatalf("unexpected error: %v\nstderr=%s", err, errOut)
	}
	if strings.Contains(out, "merged PR") || !strings.Contains(out, "Filed request hr-9") {
		t.Fatalf("stdout = %q; a filed request must not read as a merge", out)
	}
}

func TestPRMergeOmitsMissingMethodFromOutput(t *testing.T) {
	cfg := setConfigEnv(t)
	srv, _ := prMergeServer(t, trackedPR, http.StatusOK, `{"ok":true,"prNumber":42}`)
	writeRunFileFor(t, cfg, srv)

	out, errOut, err := executeCLI(t, aliveDeps(), "pr", "merge", "42")
	if err != nil {
		t.Fatalf("unexpected error: %v\nstderr=%s", err, errOut)
	}
	if out != "merged PR #42\n" {
		t.Fatalf("stdout = %q, want %q", out, "merged PR #42\n")
	}
}

func TestPRMergeStopsWhenThePRIsNotTrackedOrHasNoHeadYet(t *testing.T) {
	cfg := setConfigEnv(t)
	for name, tc := range map[string]struct {
		summary string
		want    string
	}{
		"no head commit observed": {`{"prNumber":42,"prUrl":"https://github.com/acme/repo/pull/42","headSha":"","state":"open"}`, "no known head commit"},
		"no url":                  {`{"prNumber":42,"prUrl":"","headSha":"` + prHeadSHA + `","state":"open"}`, "no known head commit"},
	} {
		srv, calls := prMergeServer(t, tc.summary, http.StatusOK, `{"ok":true}`)
		writeRunFileFor(t, cfg, srv)
		_, _, err := executeCLI(t, aliveDeps(), "pr", "merge", "42")
		if err == nil || !strings.Contains(err.Error(), tc.want) || calls.mergePath != "" {
			t.Errorf("%s: err = %v, merge call %q; the merge must not be attempted", name, err, calls.mergePath)
		}
	}
}

func TestPRMergeRejectsInvalidNumber(t *testing.T) {
	setConfigEnv(t)

	for _, number := range []string{"0", "-1", "abc", "#"} {
		t.Run(number, func(t *testing.T) {
			_, _, err := executeCLI(t, aliveDeps(), "pr", "merge", number)
			if got := ExitCode(err); got != 2 {
				t.Fatalf("exit code = %d, want 2; err=%v", got, err)
			}
		})
	}
}

func TestPRMergeRequiresExactlyOneArgument(t *testing.T) {
	setConfigEnv(t)

	for _, args := range [][]string{
		{"pr", "merge"},
		{"pr", "merge", "42", "43"},
	} {
		_, _, err := executeCLI(t, aliveDeps(), args...)
		if got := ExitCode(err); got != 2 {
			t.Fatalf("args = %v, exit code = %d, want 2; err=%v", args, got, err)
		}
	}
}

func TestPRMergeSurfacesDaemonError(t *testing.T) {
	cfg := setConfigEnv(t)
	srv, _ := prMergeServer(t, trackedPR, http.StatusConflict, `{"message":"PR is not mergeable","code":"PR_NOT_MERGEABLE","requestId":"req-1"}`)
	writeRunFileFor(t, cfg, srv)

	_, _, err := executeCLI(t, aliveDeps(), "pr", "merge", "42")
	if got := ExitCode(err); got != 1 {
		t.Fatalf("exit code = %d, want 1; err=%v", got, err)
	}
	for _, want := range []string{"PR is not mergeable", "PR_NOT_MERGEABLE", "req-1"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %q, want %q", err, want)
		}
	}
}

func TestPRMergeSurfacesAnUnknownPR(t *testing.T) {
	cfg := setConfigEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"Unknown PR","code":"PR_NOT_FOUND","requestId":"req-2"}`)
	}))
	t.Cleanup(srv.Close)
	writeRunFileFor(t, cfg, srv)
	_, _, err := executeCLI(t, aliveDeps(), "pr", "merge", "99")
	if err == nil || !strings.Contains(err.Error(), "PR_NOT_FOUND") {
		t.Fatalf("err = %v", err)
	}
}

func TestPRResolveCommentsPostsIDs(t *testing.T) {
	cfg := setConfigEnv(t)
	srv, capture := reviewServer(t, http.StatusOK, `{"ok":true,"resolved":2}`)
	writeRunFileFor(t, cfg, srv)

	out, errOut, err := executeCLI(t, aliveDeps(), "pr", "resolve-comments", "42", "thread-1", "thread-2")
	if err != nil {
		t.Fatalf("unexpected error: %v\nstderr=%s", err, errOut)
	}
	if capture.method != http.MethodPost || capture.path != "/api/v1/prs/42/resolve-comments" {
		t.Fatalf("request = %s %s", capture.method, capture.path)
	}
	var req resolveCommentsRequest
	if err := json.Unmarshal([]byte(capture.body), &req); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(req.CommentIDs) != 2 || req.CommentIDs[0] != "thread-1" || req.CommentIDs[1] != "thread-2" {
		t.Fatalf("comment ids = %v", req.CommentIDs)
	}
	if !strings.Contains(out, "resolved 2 review thread(s) on PR #42") {
		t.Fatalf("stdout = %q", out)
	}
}

func TestPRResolveCommentsAllowsNoIDs(t *testing.T) {
	cfg := setConfigEnv(t)
	srv, capture := reviewServer(t, http.StatusOK, `{"ok":true,"resolved":3}`)
	writeRunFileFor(t, cfg, srv)

	if _, errOut, err := executeCLI(t, aliveDeps(), "pr", "resolve-comments", "42"); err != nil {
		t.Fatalf("unexpected error: %v\nstderr=%s", err, errOut)
	}
	if strings.TrimSpace(capture.body) != "{}" {
		t.Fatalf("body = %q, want {}", capture.body)
	}
}

func TestPRResolveCommentsRequiresPRNumber(t *testing.T) {
	setConfigEnv(t)

	_, _, err := executeCLI(t, aliveDeps(), "pr", "resolve-comments")
	if got := ExitCode(err); got != 2 {
		t.Fatalf("exit code = %d, want 2; err=%v", got, err)
	}
}
