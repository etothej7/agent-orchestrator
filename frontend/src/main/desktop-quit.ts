type QuitEvent = { preventDefault(): void };

/** How long a quit may sit idle before the next one is treated as a fresh request. */
export const QUIT_FLOW_STALE_MS = 20_000;

/**
 * Whether closing the last window quits AO. Never on macOS: the window goes, AO keeps
 * running in the Dock and menu-bar tray (its daemon, mobile and remote access stay up).
 * Quit and Cmd-Q are different: they are a full exit on every platform.
 */
export function shouldQuitWhenAllWindowsClosed(platform: NodeJS.Platform): boolean {
	return platform !== "darwin";
}

/**
 * Quit is a real exit. Nothing here hides AO to the tray. Two things can hold a quit back:
 *  - the unsaved-draft confirmation, for every quit source;
 *  - the "your agents will pause" / "your Mac will sleep" warnings, ONLY for a quit the person asked
 *    for (the app menu's Quit / Cmd-Q and the tray's Quit AO Completely, via requestUserQuit).
 * An AppleEvent or installer quit, logout, an OS shutdown and an update relaunch are never blocked
 * by those warnings. A quit that was cancelled leaves nothing behind: the next one starts clean.
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
	/** Clock for the staleness window; defaults to Date.now. */
	now?: () => number;
}) {
	const now = options.now ?? Date.now;
	let draftLeaveConfirmed = false;
	let relaunchRequested = false;
	/** One of our own entry points started this quit (as opposed to the OS or another app). */
	let inFlight = false;
	let userInitiated = false;
	let warningsPassed = false;
	let checkingWarnings = false;
	/** An unattended update quit is under way: no dialog may open on its account, ever. */
	let updateQuitInFlight = false;
	let lastActivity = 0;

	/** Forget everything about the quit in progress. */
	const resetFlow = () => {
		relaunchRequested = false;
		userInitiated = false;
		warningsPassed = false;
		inFlight = false;
		updateQuitInFlight = false;
	};
	const touch = () => {
		lastActivity = now();
	};

	return {
		/**
		 * The `before-quit` handler. True when the quit was held for the draft confirmation.
		 * A quit that did not come through one of the methods below (an AppleEvent, a logout) is
		 * a fresh, non-user quit: any state left by an earlier cancelled one is discarded first.
		 */
		handleBeforeQuit(event: QuitEvent): boolean {
			// A quit that passed our handlers but was stopped elsewhere (a window that would not unload)
			// leaves its flow behind with no event to say so; it is discarded once it has gone quiet.
			if (inFlight && !checkingWarnings && now() - lastActivity > QUIT_FLOW_STALE_MS) resetFlow();
			if (!inFlight && !checkingWarnings) resetFlow();
			touch();
			if (updateQuitInFlight) {
				// An unattended update quit never asks. A draft that appeared since it began holds the quit,
				// silently; the update watcher sees the draft and defers until it is gone.
				if (options.draftRiskCount() > 0) {
					event.preventDefault();
					resetFlow();
					return true;
				}
				return false;
			}
			if (options.draftRiskCount() === 0 || draftLeaveConfirmed) return false;
			event.preventDefault();
			if (options.confirmDraftLeave()) {
				draftLeaveConfirmed = true;
				options.quit();
			} else {
				// The quit was called off, so everything that rode on it is too.
				resetFlow();
			}
			return true;
		},
		/**
		 * The `before-quit` handler, after the draft guard: a quit the person asked for while agents work
		 * or Stay Awake holds the lid pauses for a Keep Running / Quit Anyway choice. True when held.
		 */
		holdForWarnings(event: QuitEvent): boolean {
			if (!userInitiated || warningsPassed || options.isUpdateRestart()) return false;
			event.preventDefault();
			if (checkingWarnings) return true;
			checkingWarnings = true;
			void options
				.confirmFullQuit()
				// A failed check must not trap the person in a running app.
				.catch(() => true)
				.then((proceed) => {
					checkingWarnings = false;
					if (!proceed) {
						// Keep Running: nothing is left armed, and the next quit asks afresh.
						resetFlow();
						return;
					}
					warningsPassed = true;
					touch();
					options.quit();
				});
			return true;
		},
		/** A draft confirmed through the dialog; closing the window must not ask again. */
		draftLeaveConfirmed(): boolean {
			return draftLeaveConfirmed;
		},
		/** The drafts changed or the window went away: the next quit asks afresh. */
		resetDraftConfirmation(): void {
			draftLeaveConfirmed = false;
		},
		/** The app menu's Quit, Cmd-Q and the tray's Quit AO Completely: a quit the person asked for. */
		requestUserQuit(): void {
			resetFlow();
			inFlight = true;
			userInitiated = true;
			touch();
			options.quit();
		},
		/**
		 * The update runner asked for a quit (see update-quit-request.ts). Never a dialog: unsent drafts defer
		 * it ("deferred": nothing happened, ask again later) instead of asking, and the quit never warns or
		 * relaunches. A draft that appears after this check holds the quit in `handleBeforeQuit`, also silently.
		 */
		quitForUpdate(): "quitting" | "deferred" {
			resetFlow();
			if (options.draftRiskCount() > 0) return "deferred";
			inFlight = true;
			updateQuitInFlight = true;
			touch();
			options.quit();
			return "quitting";
		},
		/** True while an unattended update quit is under way: window handlers must not open a dialog. */
		isUpdateQuitInFlight(): boolean {
			return updateQuitInFlight;
		},
		/** The OS is shutting down: quit now, never warn. */
		quitForShutdown(): void {
			resetFlow();
			inFlight = true;
			touch();
			options.quit();
		},
		/**
		 * Update retry: start a fresh AO after this one exits, never warning. The draft confirmation
		 * comes first, and the relaunch itself is only armed at `will-quit` (see handleWillQuit), so
		 * nothing that cancels the quit afterwards can leave a stray relaunch behind.
		 */
		relaunchAndQuit(): void {
			resetFlow();
			if (options.draftRiskCount() > 0 && !draftLeaveConfirmed) {
				if (!options.confirmDraftLeave()) return;
				draftLeaveConfirmed = true;
			}
			inFlight = true;
			relaunchRequested = true;
			touch();
			options.quit();
		},
		/** The `will-quit` handler: the quit can no longer be stopped, so arm a requested relaunch now. */
		handleWillQuit(): void {
			const relaunch = relaunchRequested;
			resetFlow();
			if (relaunch) options.relaunch();
		},
	};
}
