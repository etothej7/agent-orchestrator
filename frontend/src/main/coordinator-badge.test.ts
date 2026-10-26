import { describe, expect, it, vi } from "vitest";
import { createCoordinatorBadge } from "./coordinator-badge";

type N = { type: string; purpose?: string; status: string };
const ask: N = { type: "coordinator_message", purpose: "blocking_decision", status: "unread" };
const unreadTell: N = { type: "coordinator_message", purpose: "verified_result", status: "unread" };
const readTell: N = { type: "coordinator_message", purpose: "stopped_work", status: "read" };
const agentUnread: N = { type: "needs_input", status: "unread" };

/** A daemon whose notification list the test edits; no window or renderer exists anywhere in this file. */
function setup(initial: { open: number; notifications: N[] }, options: { port?: number | undefined } = { port: 3001 }) {
	let server: { open: number; notifications: N[] } | "down" = initial;
	const shown: number[] = [];
	const ticks: Array<() => void> = [];
	let port = options.port;
	const badge = createCoordinatorBadge({
		getPort: () => port,
		fetchJson: async () => (server === "down" ? undefined : { unresolvedAskCount: server.open, notifications: server.notifications }),
		setBadge: (count) => shown.push(count),
		setInterval: (callback) => { ticks.push(callback); return ticks.length; },
		clearInterval: () => { ticks.length = 0; },
	});
	return {
		badge, shown, ticks,
		serverIs: (next: { open: number; notifications: N[] } | "down") => { server = next; },
		setPort: (next: number | undefined) => { port = next; },
		tick: async () => { ticks.at(-1)?.(); await new Promise((resolve) => setTimeout(resolve, 0)); },
	};
}

describe("coordinator Dock badge (main process, no window)", () => {
	it("is set from the daemon's state as soon as it starts: open asks plus unread tells", async () => {
		const { badge, shown } = setup({ open: 2, notifications: [ask, ask, unreadTell, readTell, agentUnread] });
		badge.start();
		await badge.refresh();
		expect(shown).toEqual([3]);
	});

	it("a new ask raises it on the next poll; no renderer is involved", async () => {
		const t = setup({ open: 0, notifications: [] });
		t.badge.start();
		await t.badge.refresh();
		t.serverIs({ open: 1, notifications: [ask] });
		await t.tick();
		expect(t.shown).toEqual([0, 1]);
	});

	it("answering a question lowers it, and so does reading a tell", async () => {
		const t = setup({ open: 1, notifications: [ask, unreadTell] });
		t.badge.start();
		await t.badge.refresh();
		expect(t.shown).toEqual([2]);
		t.serverIs({ open: 0, notifications: [{ ...ask, status: "read" }, unreadTell] }); // answered
		await t.tick();
		t.serverIs({ open: 0, notifications: [{ ...unreadTell, status: "read" }] }); // seen in the chat
		await t.tick();
		expect(t.shown).toEqual([2, 1, 0]);
	});

	it("is right after a relaunch: a fresh instance reads the server, not the old badge", async () => {
		const server = { open: 1, notifications: [unreadTell] };
		const first = setup(server);
		first.badge.start();
		await first.badge.refresh();
		first.badge.stop();
		const second = setup(server);
		second.badge.start();
		await second.badge.refresh();
		expect(second.shown).toEqual([2]);
	});

	it("does not touch the badge when nothing changed", async () => {
		const t = setup({ open: 1, notifications: [ask] });
		t.badge.start();
		await t.badge.refresh();
		await t.tick();
		await t.tick();
		expect(t.shown).toEqual([1]);
	});

	it("keeps what is shown while the daemon is down, starting or slow, and recovers after", async () => {
		const t = setup({ open: 1, notifications: [ask] });
		t.badge.start();
		await t.badge.refresh();
		t.serverIs("down");
		await t.tick();
		t.setPort(undefined);
		await t.tick();
		expect(t.shown).toEqual([1]);
		t.setPort(3001);
		t.serverIs({ open: 0, notifications: [] });
		await t.tick();
		expect(t.shown).toEqual([1, 0]);
	});

	it("folds refresh requests that arrive during a read into one follow-up read", async () => {
		const fetchJson = vi.fn(async () => ({ unresolvedAskCount: 0, notifications: [] }));
		const badge = createCoordinatorBadge({ getPort: () => 1, fetchJson, setBadge: () => undefined, setInterval: () => 1, clearInterval: () => undefined });
		const first = badge.refresh();
		void badge.refresh();
		void badge.refresh();
		await first;
		expect(fetchJson).toHaveBeenCalledTimes(2);
	});

	it("stop() ends the polling", () => {
		const t = setup({ open: 0, notifications: [] });
		t.badge.start();
		expect(t.ticks).toHaveLength(1);
		t.badge.stop();
		expect(t.ticks).toHaveLength(0);
	});

	it("ignores an answer that is not a notification list", async () => {
		const shown: number[] = [];
		const badge = createCoordinatorBadge({ getPort: () => 1, fetchJson: async () => ({ error: "nope" }), setBadge: (n) => shown.push(n), setInterval: () => 1, clearInterval: () => undefined });
		await badge.refresh();
		expect(shown).toEqual([]);
	});
});
