package mobilebridge

import (
	"errors"
	"os/exec"
	"testing"
)

func TestResolveTailscalePrefersPATH(t *testing.T) {
	look := func(name string) (string, error) { return "/somewhere/on/path/tailscale", nil }
	stat := func(string) bool { t.Fatal("fallback probed although PATH had the binary"); return false }
	if got := resolveTailscale(look, stat); got != "/somewhere/on/path/tailscale" {
		t.Errorf("got %q, want the PATH hit", got)
	}
}

func TestResolveTailscaleFallbackOrder(t *testing.T) {
	notOnPath := func(string) (string, error) { return "", exec.ErrNotFound }
	cases := []struct {
		name   string
		exists map[string]bool
		want   string
	}{
		{"usr local first", map[string]bool{
			"/usr/local/bin/tailscale":                             true,
			"/opt/homebrew/bin/tailscale":                          true,
			"/Applications/Tailscale.app/Contents/MacOS/Tailscale": true,
		}, "/usr/local/bin/tailscale"},
		{"homebrew second", map[string]bool{
			"/opt/homebrew/bin/tailscale":                          true,
			"/Applications/Tailscale.app/Contents/MacOS/Tailscale": true,
		}, "/opt/homebrew/bin/tailscale"},
		{"app bundle last", map[string]bool{
			"/Applications/Tailscale.app/Contents/MacOS/Tailscale": true,
		}, "/Applications/Tailscale.app/Contents/MacOS/Tailscale"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveTailscale(notOnPath, func(p string) bool { return tc.exists[p] })
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// With nothing found the bare name is returned, so the exec error is the usual
// "executable not found" and callers treat it as no CLI.
func TestResolveTailscaleNothingFoundKeepsBareName(t *testing.T) {
	got := resolveTailscale(
		func(string) (string, error) { return "", errors.New("not found") },
		func(string) bool { return false },
	)
	if got != "tailscale" {
		t.Errorf("got %q, want the bare name", got)
	}
}
