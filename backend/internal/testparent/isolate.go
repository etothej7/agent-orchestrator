package testparent

import "testing"

// IsolateHome points HOME and the XDG base directories at fresh temporary
// directories for the rest of the test. Use it in any non-live test that can
// launch a provider binary found on the developer's PATH, so that binary
// cannot read or write the real account, config, or session state.
//
// Do not use it in the AO_LIVE_* tests: they exist to run against the real
// local account.
func IsolateHome(t testing.TB) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	for _, name := range []string{"XDG_DATA_HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"} {
		t.Setenv(name, t.TempDir())
	}
}
