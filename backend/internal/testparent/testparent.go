// Package testparent is a test-only helper for the chat-host entry points that
// test binaries expose from their TestMain functions.
//
// A production chat host deliberately outlives its controller, but a helper
// started by a test must not outlive that test binary, however the binary dies
// (SIGKILL, a test timeout panic, a crash). Otherwise the host, and the
// provider process it supervises, are left running with no owner.
//
// It relies on the parent pid changing (to init) when the parent dies, which
// does not happen on Windows; there it is a no-op.
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
	// Read the parent once, before any goroutine runs, so a parent that died
	// before this call (we were already re-parented to init) is not mistaken
	// for the live one.
	parent := os.Getppid()
	if parent == 1 {
		cancel()
		return ctx
	}
	go cancelWhenParentChanges(parent, cancel)
	return ctx
}

// cancelWhenParentChanges calls cancel once the helper's direct parent is no
// longer parent. The host is detached with Setsid, not a double fork, so the
// test binary stays its direct parent for as long as it lives.
func cancelWhenParentChanges(parent int, cancel context.CancelFunc) {
	for {
		time.Sleep(pollInterval)
		if os.Getppid() != parent {
			cancel()
			return
		}
	}
}
