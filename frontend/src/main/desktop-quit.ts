type QuitEvent = { preventDefault(): void };

/**
 * Whether closing the last window quits AO. Never on macOS: the window goes, AO keeps
 * running in the Dock and menu-bar tray (its daemon, mobile and remote access stay up).
 * Quit and Cmd-Q are different: they are a full exit on every platform.
 */
export function shouldQuitWhenAllWindowsClosed(platform: NodeJS.Platform): boolean {
	return platform !== "darwin";
}

/**
 * Quit is a real exit. Nothing here hides AO to the tray; the only thing that can hold
 * a quit back is the unsaved-draft confirmation, and declining it leaves AO running.
 */
export function createDesktopQuitController(options: {
	/** Boundaries (unsent drafts, edits) that would be lost by leaving. */
	draftRiskCount(): number;
	/** Asks the person; true means leave anyway. */
	confirmDraftLeave(): boolean;
	quit(): void;
	relaunch(): void;
}) {
	let draftLeaveConfirmed = false;
	let relaunchRequested = false;

	return {
		/** The `before-quit` handler. True when the quit was held for the draft confirmation. */
		handleBeforeQuit(event: QuitEvent): boolean {
			if (options.draftRiskCount() === 0 || draftLeaveConfirmed) return false;
			event.preventDefault();
			if (options.confirmDraftLeave()) {
				draftLeaveConfirmed = true;
				options.quit();
			} else {
				// The quit was called off, so a relaunch that rode on it is too.
				relaunchRequested = false;
			}
			return true;
		},
		/** A quit was confirmed through the draft dialog; closing the window must not ask again. */
		draftLeaveConfirmed(): boolean {
			return draftLeaveConfirmed;
		},
		/** The drafts changed or the window went away: the next quit asks afresh. */
		resetDraftConfirmation(): void {
			draftLeaveConfirmed = false;
		},
		/** The tray's "Quit AO Completely" and the OS shutdown. */
		quit(): void {
			options.quit();
		},
		/**
		 * Update retry: start a fresh AO after this one exits. The draft confirmation comes
		 * first, and the relaunch itself is only armed at `will-quit` (see handleWillQuit), so
		 * nothing that cancels the quit afterwards can leave a stray relaunch behind.
		 */
		relaunchAndQuit(): void {
			if (options.draftRiskCount() > 0 && !draftLeaveConfirmed) {
				if (!options.confirmDraftLeave()) return;
				draftLeaveConfirmed = true;
			}
			relaunchRequested = true;
			options.quit();
		},
		/** The `will-quit` handler: the quit can no longer be stopped, so arm a requested relaunch now. */
		handleWillQuit(): void {
			if (!relaunchRequested) return;
			relaunchRequested = false;
			options.relaunch();
		},
	};
}
