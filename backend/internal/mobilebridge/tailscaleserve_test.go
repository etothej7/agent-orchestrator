package mobilebridge

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const serveStatusJSON = `{
  "TCP": {"443": {"HTTPS": true}},
  "Web": {"prasads-macbook-pro.tail057d04.ts.net:443": {
    "Handlers": {"/": {"Proxy": "http://127.0.0.1:54014"}}}}
}`

type recordingRunner struct {
	calls [][]string
	out   string
	err   error
}

func (r *recordingRunner) run(ctx context.Context, args ...string) ([]byte, error) {
	r.calls = append(r.calls, args)
	return []byte(r.out), r.err
}

func TestServeApplyIssuesExpectedArgv(t *testing.T) {
	r := &recordingRunner{}
	s := &Serve{Run: r.run}
	if err := s.Apply(context.Background(), 54014); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(r.calls) != 2 || strings.Join(r.calls[0], " ") != "serve status --json" {
		t.Fatalf("calls = %v, want a read-only status probe then one apply", r.calls)
	}
	got := strings.Join(r.calls[1], " ")
	want := "serve --bg --https=443 http://127.0.0.1:54014"
	if got != want {
		t.Errorf("argv = %q, want %q", got, want)
	}
}

func TestServeClearIssuesOff(t *testing.T) {
	r := &recordingRunner{}
	s := &Serve{Run: r.run}
	if err := s.Clear(context.Background()); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	// Nothing is configured, so there is nothing of AO's to remove: the probe
	// is the only call and the node-global `off` is never issued.
	if len(r.calls) != 1 || strings.Join(r.calls[0], " ") != "serve status --json" {
		t.Errorf("calls = %v, want only the read-only probe", r.calls)
	}
}

// serveStatusMultiSiteJSON simulates a tailnet where the user also runs an
// unrelated `tailscale serve --https=8443 ...` alongside AO's :443 proxy.
// Target must key off the ":443" suffix, not just "some Web entry", or Go's
// randomized map iteration could return the unrelated handler's port instead.
const serveStatusMultiSiteJSON = `{
  "TCP": {"443": {"HTTPS": true}, "8443": {"HTTPS": true}},
  "Web": {
    "prasads-macbook-pro.tail057d04.ts.net:443": {
      "Handlers": {"/": {"Proxy": "http://127.0.0.1:54014"}}},
    "prasads-macbook-pro.tail057d04.ts.net:8443": {
      "Handlers": {"/": {"Proxy": "http://127.0.0.1:9999"}}}
  }
}`

func TestServeTargetIgnoresNon443Handlers(t *testing.T) {
	r := &recordingRunner{out: serveStatusMultiSiteJSON}
	s := &Serve{Run: r.run}
	if got := s.Target(context.Background()); got != 54014 {
		t.Errorf("Target = %d, want 54014 (must ignore the :8443 handler)", got)
	}
}

func TestServeTargetParsesProxyPort(t *testing.T) {
	r := &recordingRunner{out: serveStatusJSON}
	s := &Serve{Run: r.run}
	if got := s.Target(context.Background()); got != 54014 {
		t.Errorf("Target = %d, want 54014", got)
	}
}

// An unset proxy, a failed call, and unparseable output are all "no target",
// which the caller renders as "proxy not active" rather than as an error.
func TestServeTargetZeroWhenUnset(t *testing.T) {
	for name, r := range map[string]*recordingRunner{
		"empty config": {out: `{"TCP":{},"Web":{}}`},
		"cli error":    {out: "", err: errors.New("exit status 1")},
		"malformed":    {out: "not json"},
	} {
		t.Run(name, func(t *testing.T) {
			s := &Serve{Run: r.run}
			if got := s.Target(context.Background()); got != 0 {
				t.Errorf("Target = %d, want 0", got)
			}
		})
	}
}

// Apply must surface CLI failure so the caller can report serve_failed rather
// than advertising a proxy that was never created.
func TestServeApplyPropagatesError(t *testing.T) {
	r := &recordingRunner{err: errors.New("exit status 1")}
	s := &Serve{Run: r.run}
	if err := s.Apply(context.Background(), 3011); err == nil {
		t.Error("Apply returned nil, want the CLI error")
	}
}

func TestServeHTTPSInUse(t *testing.T) {
	cases := []struct {
		name string
		out  string
		err  error
		want bool
		bad  bool
	}{
		{"empty object", `{}`, nil, false, false},
		{"empty output", ``, nil, false, false},
		{"https 443 proxy", serveStatusJSON, nil, true, false},
		{"tcp forward on 443", `{"TCP": {"443": {"TCPForward": "127.0.0.1:9"}}}`, nil, true, false},
		{"only another port", `{"TCP": {"8443": {"HTTPS": true}}, "Web": {"h.ts.net:8443": {"Handlers": {"/": {"Proxy": "http://127.0.0.1:9"}}}}}`, nil, false, false},
		{"static text on 443", foreignStatic, nil, true, false},
		{"cli error", ``, errors.New("exit 1"), false, true},
		{"garbage", `not json`, nil, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &recordingRunner{out: tc.out, err: tc.err}
			got, err := (&Serve{Run: r.run}).HTTPSInUse(context.Background())
			if (err != nil) != tc.bad {
				t.Fatalf("err = %v, want error=%v", err, tc.bad)
			}
			if got != tc.want {
				t.Errorf("in use = %v, want %v", got, tc.want)
			}
			if want := "serve status --json"; strings.Join(r.calls[0], " ") != want {
				t.Errorf("argv = %q, want a read-only status call", strings.Join(r.calls[0], " "))
			}
		})
	}
}

// seqRunner answers `serve status --json` with a fixed config and records every
// other call, so tests can assert the node-global config is never mutated.
type seqRunner struct {
	status string
	err    error
	writes [][]string
}

func (r *seqRunner) run(ctx context.Context, args ...string) ([]byte, error) {
	if len(args) >= 2 && args[0] == "serve" && args[1] == "status" {
		return []byte(r.status), r.err
	}
	r.writes = append(r.writes, args)
	return nil, nil
}

const (
	ownedProxyJSON = `{"TCP":{"443":{"HTTPS":true}},"Web":{"h.tail1.ts.net:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:4000"}}}}}`
	foreignStatic  = `{"TCP":{"443":{"HTTPS":true}},"Web":{"h.tail1.ts.net:443":{"Handlers":{"/":{"Text":"hello"}}}}}`
	foreignPath    = `{"TCP":{"443":{"HTTPS":true}},"Web":{"h.tail1.ts.net:443":{"Handlers":{"/":{"Path":"/Users/x/site"}}}}}`
	foreignTCP     = `{"TCP":{"443":{"TCPForward":"127.0.0.1:22"}}}`
	foreignMulti   = `{"TCP":{"443":{"HTTPS":true}},"Web":{"h.tail1.ts.net:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:4000"},"/api":{"Proxy":"http://127.0.0.1:5000"}}}}}`
	foreignRemote  = `{"TCP":{"443":{"HTTPS":true}},"Web":{"h.tail1.ts.net:443":{"Handlers":{"/":{"Proxy":"http://10.0.0.9:4000"}}}}}`
	funnelOnOwned  = `{"TCP":{"443":{"HTTPS":true}},"Web":{"h.tail1.ts.net:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:4000"}}}},"AllowFunnel":{"h.tail1.ts.net:443":true}}`
	// A `tailscale serve` run in the foreground keeps its config under
	// "Foreground", keyed by session, beside the (empty) background config.
	foregroundWeb    = `{"Foreground":{"sess1":{"TCP":{"443":{"HTTPS":true}},"Web":{"h.tail1.ts.net:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:4000"}}}}}}}`
	foregroundFunnel = `{"Foreground":{"sess1":{"AllowFunnel":{"h.tail1.ts.net:443":true}}}}`
	foregroundOther  = `{"Foreground":{"sess1":{"TCP":{"8443":{"HTTPS":true}},"Web":{"h.tail1.ts.net:8443":{"Handlers":{"/":{"Text":"x"}}}}}}}`
	funnelOnEmpty    = `{"AllowFunnel":{"h.tail1.ts.net:443":true}}`
)

func TestServeApplyOnlyTakesEmptyOrOwnedConfig(t *testing.T) {
	cases := []struct {
		name   string
		status string
		owned  []int
		want   error // nil means the apply must happen
	}{
		{"empty object", `{}`, nil, nil},
		{"empty output", ``, nil, nil},
		{"our previous proxy", ownedProxyJSON, []int{4000}, nil},
		{"our previous proxy, old port recorded among several", ownedProxyJSON, []int{9, 4000}, nil},
		{"proxy to a port AO never applied", ownedProxyJSON, []int{4001}, ErrForeignServe},
		{"proxy with no ownership record", ownedProxyJSON, nil, ErrForeignServe},
		{"static text", foreignStatic, []int{4000}, ErrForeignServe},
		{"static path", foreignPath, []int{4000}, ErrForeignServe},
		{"tcp forward", foreignTCP, []int{4000}, ErrForeignServe},
		{"several handlers", foreignMulti, []int{4000}, ErrForeignServe},
		{"non-loopback proxy", foreignRemote, []int{4000}, ErrForeignServe},
		{"funnel on our own proxy", funnelOnOwned, []int{4000}, ErrFunnelExposed},
		{"funnel flag on an otherwise empty :443", funnelOnEmpty, nil, ErrFunnelExposed},
		// A foreground session is someone's live config even when it looks like
		// AO's own shape, so it is never taken over.
		{"foreground serve on 443", foregroundWeb, []int{4000}, ErrForeignServe},
		{"foreground funnel on 443", foregroundFunnel, nil, ErrFunnelExposed},
		{"foreground serve on another port", foregroundOther, nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &seqRunner{status: tc.status}
			err := (&Serve{Run: r.run}).Apply(context.Background(), 5000, tc.owned...)
			if !errors.Is(err, tc.want) || (tc.want == nil && err != nil) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if tc.want != nil && len(r.writes) != 0 {
				t.Errorf("refused but still mutated the serve config: %v", r.writes)
			}
			if tc.want == nil && (len(r.writes) != 1 || strings.Join(r.writes[0], " ") != "serve --bg --https=443 http://127.0.0.1:5000") {
				t.Errorf("writes = %v, want exactly one apply", r.writes)
			}
		})
	}
}

func TestServeApplyRefusesWhenConfigUnreadable(t *testing.T) {
	for name, r := range map[string]*seqRunner{
		"cli error": {err: errors.New("exit 1")},
		"garbage":   {status: "not json"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := (&Serve{Run: r.run}).Apply(context.Background(), 5000, 5000); err == nil {
				t.Fatal("Apply succeeded without being able to read the current config")
			}
			if len(r.writes) != 0 {
				t.Errorf("mutated an unreadable config: %v", r.writes)
			}
		})
	}
}

func TestServeClearOnlyRemovesWhatAOApplied(t *testing.T) {
	cases := []struct {
		name      string
		status    string
		owned     []int
		wantWrite bool
	}{
		{"our proxy", ownedProxyJSON, []int{4000}, true},
		{"our proxy with funnel flag", funnelOnOwned, []int{4000}, true},
		{"already empty", `{}`, []int{4000}, false},
		{"proxy to someone else's port", ownedProxyJSON, []int{4001}, false},
		{"static", foreignStatic, []int{4000}, false},
		{"tcp forward", foreignTCP, []int{4000}, false},
		{"no ownership record", ownedProxyJSON, nil, false},
		{"foreground serve on 443", foregroundWeb, []int{4000}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &seqRunner{status: tc.status}
			if err := (&Serve{Run: r.run}).Clear(context.Background(), tc.owned...); err != nil {
				t.Fatalf("Clear: %v", err)
			}
			if got := len(r.writes) == 1 && strings.Join(r.writes[0], " ") == "serve --https=443 off"; got != tc.wantWrite {
				t.Errorf("wrote = %v (%v), want %v", got, r.writes, tc.wantWrite)
			}
			if !tc.wantWrite && len(r.writes) != 0 {
				t.Errorf("mutated a config AO does not own: %v", r.writes)
			}
		})
	}
}

// If the config cannot be read, Clear must not guess: it reports the failure so
// the caller keeps its cleanup pending, and issues no `off`.
func TestServeClearUnreadableConfigIsAnErrorNotAnOff(t *testing.T) {
	r := &seqRunner{err: errors.New("exit 1")}
	if err := (&Serve{Run: r.run}).Clear(context.Background(), 4000); err == nil {
		t.Fatal("want an error")
	}
	if len(r.writes) != 0 {
		t.Errorf("issued %v", r.writes)
	}
}
