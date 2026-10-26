import { coordinatorBadgeCount, type BadgePage } from "../shared/coordinator-badge";

export const COORDINATOR_BADGE_POLL_MS = 4000;
export const COORDINATOR_BADGE_TIMEOUT_MS = 3000;

/**
 * Keeps the Dock badge right without any window. It lives in the main process on purpose: with Quit
 * fully quitting but window close leaving AO running, there may be no renderer at all, and the badge
 * is exactly what Ed relies on then. It reads the daemon's notifications on a short poll, and
 * `refresh()` is called straight away when the renderer has just read a result or answered a question.
 */
export function createCoordinatorBadge(deps: {
	/** The daemon's port, or undefined while it is not ready. */
	getPort(): number | undefined;
	/** GET a daemon URL as JSON; undefined when it did not answer. */
	fetchJson(url: string, timeoutMs: number): Promise<unknown | undefined>;
	setBadge(count: number): void;
	setInterval?: (callback: () => void, ms: number) => unknown;
	clearInterval?: (handle: unknown) => void;
	intervalMs?: number;
}) {
	const every = deps.setInterval ?? ((callback, ms) => setInterval(callback, ms));
	const stopEvery = deps.clearInterval ?? ((handle) => clearInterval(handle as ReturnType<typeof setInterval>));
	let timer: unknown;
	let shown: number | undefined;
	let running: Promise<void> | undefined;
	let again = false;

	async function readOnce(): Promise<void> {
		const port = deps.getPort();
		// No daemon (starting, stopped, restarting) says nothing about Ed's questions: keep what is shown.
		if (port === undefined) return;
		let page: unknown;
		try {
			page = await deps.fetchJson(`http://127.0.0.1:${port}/api/v1/notifications?status=all&limit=100`, COORDINATOR_BADGE_TIMEOUT_MS);
		} catch {
			return;
		}
		if (!page || typeof page !== "object" || !Array.isArray((page as BadgePage).notifications)) return;
		const count = coordinatorBadgeCount(page as BadgePage);
		if (count === shown) return;
		shown = count;
		deps.setBadge(count);
	}

	/** Reads now. Calls that arrive while a read is in flight are folded into one follow-up read. */
	function refresh(): Promise<void> {
		if (running) {
			again = true;
			return running;
		}
		running = (async () => {
			try {
				do {
					again = false;
					await readOnce();
				} while (again);
			} finally {
				running = undefined;
			}
		})();
		return running;
	}

	return {
		start(): void {
			if (timer !== undefined) return;
			timer = every(() => void refresh(), deps.intervalMs ?? COORDINATOR_BADGE_POLL_MS);
			void refresh();
		},
		stop(): void {
			if (timer !== undefined) stopEvery(timer);
			timer = undefined;
		},
		refresh,
	};
}
