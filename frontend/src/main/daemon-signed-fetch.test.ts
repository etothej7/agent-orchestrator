import { describe, expect, it, vi } from "vitest";
import { createDaemonSignedFetch } from "./daemon-signed-fetch";
import { UI_CAPABILITY_HEADER, signUiRequestCapability } from "./ui-capability";

const KEY = "k".repeat(43);
const ORIGIN = "http://127.0.0.1:41234";
const NOW = 1_800_000_000_000;

function rig(daemonOrigin: () => string | undefined, clock = { t: NOW }) {
	const sent: { url: string; init: RequestInit }[] = [];
	const fetchImpl = vi.fn(async (url: string, init?: RequestInit) => {
		sent.push({ url, init: init ?? {} });
		return new Response(null, { status: 204 });
	});
	const daemonFetch = createDaemonSignedFetch({
		key: KEY,
		daemonOrigin,
		fetchImpl: fetchImpl as never,
		now: () => clock.t,
		sleep: async (ms) => {
			clock.t += ms;
		},
	});
	const header = (i = 0) => new Headers(sent[i]?.init.headers).get(UI_CAPABILITY_HEADER);
	return { daemonFetch, sent, header, clock };
}

describe("createDaemonSignedFetch", () => {
	it("signs a write to the verified daemon for exactly that method and path", async () => {
		const { daemonFetch, header } = rig(() => ORIGIN);
		await daemonFetch(`${ORIGIN}/api/v1/side-chats/launch/claim`, { method: "POST", body: "{}" });
		expect(header()).toBe(signUiRequestCapability(KEY, "POST", "/api/v1/side-chats/launch/claim", NOW));
	});

	it.each(["POST", "PUT", "PATCH", "DELETE", "post"])("signs %s", async (method) => {
		const { daemonFetch, header } = rig(() => ORIGIN);
		await daemonFetch(`${ORIGIN}/api/v1/awake/downloads`, { method });
		expect(header()).not.toBeNull();
	});

	it("never signs a read", async () => {
		const { daemonFetch, header, sent } = rig(() => ORIGIN);
		await daemonFetch(`${ORIGIN}/api/v1/awake`);
		await daemonFetch(`${ORIGIN}/api/v1/awake`, { method: "GET" });
		await daemonFetch(`${ORIGIN}/api/v1/awake`, { method: "HEAD" });
		expect(sent).toHaveLength(3);
		expect([header(0), header(1), header(2)]).toEqual([null, null, null]);
	});

	it("does not sign for another address than the verified daemon, nor when no daemon is verified", async () => {
		const other = rig(() => ORIGIN, { t: NOW });
		await other.daemonFetch("http://127.0.0.1:41235/api/v1/awake/downloads", { method: "POST" });
		await other.daemonFetch("http://127.0.0.1:41234.evil.example/api/v1/awake/downloads", { method: "POST" });
		await other.daemonFetch("https://example.com/x", { method: "POST" });
		expect([other.header(0), other.header(1), other.header(2)]).toEqual([null, null, null]);

		const none = rig(() => undefined);
		await none.daemonFetch(`${ORIGIN}/api/v1/awake/downloads`, { method: "POST" }, 500);
		expect(none.sent).toHaveLength(1);
		expect(none.header()).toBeNull();
	});

	it("waits for the daemon to prove itself, then signs", async () => {
		let polls = 0;
		const { daemonFetch, header, sent } = rig(() => (++polls > 3 ? ORIGIN : undefined));
		await daemonFetch(`${ORIGIN}/api/v1/side-chats/launch/claim`, { method: "POST" });
		expect(polls).toBeGreaterThan(3);
		expect(sent).toHaveLength(1);
		expect(header()).not.toBeNull();
	});

	it("gives up after the wait and sends the write unsigned for the daemon to refuse", async () => {
		const { daemonFetch, header, clock, sent } = rig(() => undefined);
		await daemonFetch(`${ORIGIN}/api/v1/side-chats/launch/retire`, { method: "POST" }, 1500);
		expect(clock.t - NOW).toBeGreaterThanOrEqual(1500);
		expect(clock.t - NOW).toBeLessThan(2000);
		expect(sent).toHaveLength(1);
		expect(header()).toBeNull();
	});

	it("replaces a capability header the caller set and keeps its other headers and body", async () => {
		const { daemonFetch, sent, header } = rig(() => ORIGIN);
		await daemonFetch(`${ORIGIN}/api/v1/awake/downloads`, {
			method: "POST",
			headers: { "content-type": "application/json", [UI_CAPABILITY_HEADER]: "forged" },
			body: '{"activeIds":[]}',
		});
		expect(header()).toBe(signUiRequestCapability(KEY, "POST", "/api/v1/awake/downloads", NOW));
		expect(new Headers(sent[0]?.init.headers).get("content-type")).toBe("application/json");
		expect(sent[0]?.init.body).toBe('{"activeIds":[]}');

		const unsigned = rig(() => undefined);
		await unsigned.daemonFetch(`${ORIGIN}/x`, { method: "POST", headers: { [UI_CAPABILITY_HEADER]: "forged" } }, 0);
		expect(unsigned.header()).toBeNull(); // a forged header is dropped even when nothing signs
	});

	it("signs the escaped path as sent, not a decoded one", async () => {
		const { daemonFetch, header } = rig(() => ORIGIN);
		await daemonFetch(`${ORIGIN}/api/v1/sessions/a%2Fb/x?appRunId=1`, { method: "POST" });
		expect(header()).toBe(signUiRequestCapability(KEY, "POST", "/api/v1/sessions/a%2Fb/x", NOW));
	});
});
