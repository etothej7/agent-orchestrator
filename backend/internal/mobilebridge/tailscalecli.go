package mobilebridge

import (
	"os"
	"os/exec"
)

// tailscaleFallbacks are the install locations probed when `tailscale` is not on
// PATH. An Electron-launched daemon inherits the GUI session's minimal PATH,
// which lacks /usr/local/bin and Homebrew, so a perfectly good install looked
// like "no CLI". Order is the preference when several exist: the CLI shim, the
// Homebrew formula, then the App Store/standalone app's own binary.
var tailscaleFallbacks = []string{
	"/usr/local/bin/tailscale",
	"/opt/homebrew/bin/tailscale",
	"/Applications/Tailscale.app/Contents/MacOS/Tailscale",
}

// resolveTailscale returns the tailscale executable to run: PATH first, then
// the known install locations, else the bare name so exec reports the standard
// not-found error. lookPath and exists are injected so tests never touch the
// real filesystem or PATH.
func resolveTailscale(lookPath func(string) (string, error), exists func(string) bool) string {
	if p, err := lookPath("tailscale"); err == nil {
		return p
	}
	for _, p := range tailscaleFallbacks {
		if exists(p) {
			return p
		}
	}
	return "tailscale"
}

func tailscaleBinary() string {
	return resolveTailscale(exec.LookPath, func(p string) bool {
		info, err := os.Stat(p)
		return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
	})
}
