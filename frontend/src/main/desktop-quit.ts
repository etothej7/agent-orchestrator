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
	/** The "your agents will pause" / "your Mac will sleep" check; true means go ahead and quit. */
	confirmFullQuit(): Promise<boolean>;
	/** The updater is restarting AO for an install: no warnings. */
	isUpdateRestart(): boolean;
}) {
	let draftLeaveConfirmed = false;
	let relaunchRequested = false;
	let skipWarnings = false;
	let warningsPassed = false;
	let checkingWarnings = false;

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
				skipWarnings = false;
			}
			return true;
		},
		/**
		 * The `before-quit` handler, after the draft guard: a person-initiated quit (Cmd-Q, Quit, the
		 * tray) while agents work or Stay Awake holds the lid pauses for a Keep Running / Quit Anyway
		 * choice. Shutdown, an update restart and a relaunch never ask. True when the quit was held.
		 */
		holdForWarnings(event: QuitEvent): boolean {
			if (skipWarnings || warningsPassed || options.isUpdateRestart()) return false;
			event.preventDefault();
			if (checkingWarnings) return true;
			checkingWarnings = true;
			void options
				.confirmFullQuit()
				// A failed check must not trap the person in a running app.
				.catch(() => true)
				.then((proceed) => {
					checkingWarnings = false;
					if (!proceed) return;
					warningsPassed = true;
					options.quit();
				});
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
		/** The tray's "Quit AO Completely" or a Cmd-Q style quit: asks about agents and sleep first. */
		quit(): void {
			options.quit();
		},
		/** The OS is shutting down: quit now, without asking about agents or sleep. */
		quitForShutdown(): void {
			skipWarnings = true;
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
			skipWarnings = true;
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
