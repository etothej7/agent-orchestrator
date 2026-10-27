// Package testparent is a test-only helper for the chat-host entry points that
// the chatdriver packages' TestMain functions expose.
//
// A production chat host deliberately outlives its controller, but a helper
// started by a test must not outlive that test binary, however the binary dies
// (SIGKILL, a test timeout panic, a crash). Otherwise the host, and the
// provider process it supervises, are left running with no owner.
package testparent

import (
	"context"
	"os"
	"time"
)

const pollInterval = 200 * time.Millisecond

// Context returns a context that is cancelled once the test process that
// spawned this helper is gone. Pass it to persistenthost.Run from a TestMain
// chat-host branch.
func Context() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	go exitWhenParentDies(cancel)
	return ctx
}

// exitWhenParentDies calls cancel once the parent test process has died. The
// helper is its direct child (the host is detached with Setsid, not a double
// fork), so the parent pid changes (to init) when the test binary dies.
func exitWhenParentDies(cancel context.CancelFunc) {
	parent := os.Getppid()
	for {
		time.Sleep(pollInterval)
		if os.Getppid() != parent {
			cancel()
			return
		}
	}
}
