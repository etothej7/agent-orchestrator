import type { BaseWindow, WebContents, WebContentsView } from "electron";

type MainWindowHost = Pick<BaseWindow, "contentView" | "getContentBounds" | "isDestroyed" | "on" | "removeListener">;

const windowResizeEvents = ["resize", "show", "restore", "enter-full-screen", "leave-full-screen"] as const;
const windowRefreshEvents = ["show", "restore", "focus"] as const;

export type WebContentsViewConstructor = new (options: {
	webPreferences: Electron.WebPreferences;
}) => WebContentsView;

export type WindowComposition = {
	shellView: WebContentsView;
	shellWebContents: WebContents;
	setOverlayOpen: (open: boolean) => void;
	/** Re-raise the shell after another native view was added above it. */
	restackShell: () => void;
	resize: () => void;
	dispose: () => void;
};

/**
 * Owns the explicit shell surface used by the native compositor.
 *
 * The native path uses a BaseWindow so there is no implicit BrowserWindow
 * renderer competing with this explicit shell surface. The main process can
 * then move the shell above/below the live browser page without a hidden blank
 * renderer covering it.
 */
export function createWindowComposition(options: {
	mainWindow: MainWindowHost;
	WebContentsView: WebContentsViewConstructor;
	preload: string;
	platform: NodeJS.Platform;
}): WindowComposition {
	const shellView = new options.WebContentsView({
		webPreferences: {
			preload: options.preload,
			contextIsolation: true,
			nodeIntegration: false,
			sandbox: true,
			transparent: true,
		},
	});
	shellView.setBackgroundColor("#00000000");
	options.mainWindow.contentView.addChildView(shellView, 0);

	let overlayOpen = false;
	let disposed = false;
	let surfaceRefreshTimer: ReturnType<typeof setTimeout> | undefined;
	let surfaceRefreshRequest: ReturnType<typeof setTimeout> | undefined;
	const resize = (): void => {
		if (disposed || options.mainWindow.isDestroyed?.()) return;
		const bounds = options.mainWindow.getContentBounds();
		shellView.setBounds({ x: 0, y: 0, width: bounds.width, height: bounds.height });
		shellView.setVisible(true);
	};
	options.mainWindow.contentView.on("bounds-changed", resize);
	// A native fullscreen/restore transition can miss the View notification or
	// leave its bounds stale. Use the window's content dimensions and events so
	// both rendering and hit testing follow the actual window size.
	for (const event of windowResizeEvents) options.mainWindow.on(event, resize);

	// Force Chromium to rebuild the shell surface after reordering it. Identical
	// bounds are ignored, so nudge by one pixel and restore on the next tick.
	// The shell stays visible throughout; hiding either native view causes a flash.
	const forceSurfaceRefresh = (): void => {
		if (disposed || options.mainWindow.isDestroyed?.()) return;
		const bounds = options.mainWindow.getContentBounds();
		if (bounds.width <= 0 || bounds.height <= 0) return;
		clearTimeout(surfaceRefreshTimer);
		shellView.setBounds({ x: 0, y: 0, width: bounds.width, height: Math.max(1, bounds.height - 1) });
		surfaceRefreshTimer = setTimeout(() => {
			surfaceRefreshTimer = undefined;
			if (disposed || options.mainWindow.isDestroyed?.()) return;
			const current = options.mainWindow.getContentBounds();
			shellView.setBounds({ x: 0, y: 0, width: current.width, height: current.height });
		}, 0);
	};
	const requestSurfaceRefresh = (): void => {
		if (disposed || options.mainWindow.isDestroyed?.()) return;
		clearTimeout(surfaceRefreshRequest);
		surfaceRefreshRequest = setTimeout(() => {
			surfaceRefreshRequest = undefined;
			forceSurfaceRefresh();
		}, 0);
	};
	// Our load listener runs before showWhenRendererLoaded shows the window.
	// Defer the nudge until after that show (and its resize callbacks), otherwise
	// the hidden surface is refreshed and the show immediately undoes the nudge.
	// Refresh on activation too: macOS can replace the surface when a background
	// launch is first focused or a minimized window is restored.
	if (options.platform === "darwin") {
		shellView.webContents.on("did-finish-load", requestSurfaceRefresh);
		for (const event of windowRefreshEvents) options.mainWindow.on(event, requestSurfaceRefresh);
	}

	const placeShell = (open: boolean): void => {
		if (open) {
			// Re-adding an existing child raises it above all page/DevTools views.
			options.mainWindow.contentView.addChildView(shellView);
		} else {
			// Index zero leaves every live native surface above the transparent shell.
			options.mainWindow.contentView.addChildView(shellView, 0);
		}
		// Restacking alone does not re-establish the shell's compositing surface:
		// on macOS the freshly-raised shell can present a stale surface that hides
		// the live page beneath it, leaving the viewport blank until some real
		// geometry change rebuilds it. (Symptom: blank on a fresh launch, but
		// correct at every size once the window has been resized or fullscreened
		// even once.) Re-applying identical bounds is a no-op, so nudge the height
		// by a pixel and restore it. Keep that repaint macOS-only: the stale
		// compositor surface has only been observed there.
		if (open && options.platform === "darwin") forceSurfaceRefresh();
	};

	const setOverlayOpen = (open: boolean): void => {
		if (overlayOpen === open) return;
		overlayOpen = open;
		placeShell(open);
	};

	// `addChildView` without an index appends on top, so a browser tab created
	// while an overlay is open (a long-lived update notice, a dialog during an
	// agent-driven `ao browser` action) would cover the raised shell until the
	// overlay toggled again. Put the shell back on top whenever that happens.
	const restackShell = (): void => {
		if (!overlayOpen || options.mainWindow.isDestroyed?.()) return;
		placeShell(true);
	};

	resize();

	return {
		shellView,
		shellWebContents: shellView.webContents,
		setOverlayOpen,
		restackShell,
		resize,
		dispose: () => {
			if (disposed) return;
			disposed = true;
			clearTimeout(surfaceRefreshTimer);
			clearTimeout(surfaceRefreshRequest);
			shellView.webContents.removeListener("did-finish-load", requestSurfaceRefresh);
			for (const event of windowRefreshEvents) options.mainWindow.removeListener(event, requestSurfaceRefresh);
			for (const event of windowResizeEvents) options.mainWindow.removeListener(event, resize);
			try {
				// Reading `contentView` on an already-destroyed BaseWindow throws
				// "Object has been destroyed". On window close the `closed` event
				// fires after the native window is gone, so both calls here can
				// throw — keep them together and out of the way of the shell
				// WebContents teardown below, which must always run.
				options.mainWindow.contentView.removeListener("bounds-changed", resize);
				options.mainWindow.contentView.removeChildView(shellView);
			} catch {
				// The BaseWindow may already have destroyed its content hierarchy.
			}
			try {
				shellView.webContents.close();
			} catch {
				// Explicit shell WebContents ownership is idempotent during teardown.
			}
		},
	};
}
