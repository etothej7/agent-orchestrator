/**
 * Whether any AO window has the keyboard focus. AO's shell is a `BaseWindow` (a `WebContentsView` inside it), which
 * `BrowserWindow.getFocusedWindow()` never returns, so that call alone is always null for the real window.
 * `BaseWindow.getFocusedWindow()` covers the shell and any `BrowserWindow` (a subclass); both are asked so a window
 * of either kind counts.
 */
export function aoWindowFocused(electron: {
	BaseWindow: { getFocusedWindow(): unknown };
	BrowserWindow: { getFocusedWindow(): unknown };
}): boolean {
	return electron.BaseWindow.getFocusedWindow() != null || electron.BrowserWindow.getFocusedWindow() != null;
}
