import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it, vi } from "vitest";
import { createKeyPossessionCheck, probeProof } from "./human-capability-probe";

const KEY = "k".repeat(43);
const ORIGIN = "http://127.0.0.1:41234";
const NONCE = "AbCdEfGhIjKlMnOpQrStUvWxYz012345";

describe("probeProof", () => {
	it("matches the daemon's vector", () => {
		const vectors = JSON.parse(readFileSync(resolve(__dirname, "../../../backend/internal/uicapability/testdata/vectors.json"), "utf8")) as {
			probe: { key: string; nonce: string; port: number; pid: number; proof: string };
		};
		const { key, nonce, port, pid, proof } = vectors.probe;
		expect(probeProof(key, nonce, port, pid)).toBe(proof);
	});

	it("depends on the key, the nonce, the port and the pid, and is not a request capability", () => {
		expect(probeProof(KEY, NONCE, 41234, 900)).not.toBe(probeProof("z".repeat(43), NONCE, 41234, 900));
		expect(probeProof(KEY, NONCE, 41234, 900)).not.toBe(probeProof(KEY, NONCE + "x", 41234, 900));
		expect(probeProof(KEY, NONCE, 41234, 900)).not.toBe(probeProof(KEY, NONCE, 41235, 900));
		expect(probeProof(KEY, NONCE, 41234, 900)).not.toBe(probeProof(KEY, NONCE, 41234, 901));
		expect(probeProof(KEY, NONCE, 41234, 900)).not.toMatch(/^v1\./);
	});
});

/** A daemon (process `pid`) that answers every probe with the proof for the port the request arrived on. */
function daemon(key: string, status = 200, pid = 900) {
	return vi.fn(async (url: string | URL | Request, init?: RequestInit) => {
		const { nonce } = JSON.parse(String(init?.body)) as { nonce: string };
		const port = Number(new URL(String(url)).port);
		return new Response(JSON.stringify({ proof: probeProof(key, nonce, port, pid) }), { status });
	});
}

async function settle() {
	await new Promise((resolve) => setTimeout(resolve, 0));
	await new Promise((resolve) => setTimeout(resolve, 0));
}

describe("createKeyPossessionCheck", () => {
	it("signs for nothing until a probe has been answered with the right proof", async () => {
		const fetchImpl = daemon(KEY);
		const check = createKeyPossessionCheck({ key: KEY, fetchImpl: fetchImpl as never, newNonce: () => NONCE });
		expect(check.isVerified(ORIGIN, 900)).toBe(false);
		check.ensure(ORIGIN, 900);
		expect(check.isVerified(ORIGIN, 900)).toBe(false); // pending: still unsigned
		await settle();
		expect(check.isVerified(ORIGIN, 900)).toBe(true);
		expect(fetchImpl).toHaveBeenCalledTimes(1);
		expect(fetchImpl.mock.calls[0]?.[0]).toBe(`${ORIGIN}/internal/human-capability/probe`);
		expect(JSON.parse(String(fetchImpl.mock.calls[0]?.[1]?.body))).toEqual({ nonce: NONCE });
	});

	it("never trusts a listener that does not hold the key (a forged run file points the app at one)", async () => {
		const check = createKeyPossessionCheck({ key: KEY, fetchImpl: daemon("z".repeat(43)) as never, newNonce: () => NONCE });
		check.ensure(ORIGIN, 900);
		await settle();
		expect(check.isVerified(ORIGIN, 900)).toBe(false);
	});

	it.each([
		["a listener that answers 404 (a daemon without a key)", () => new Response("{}", { status: 404 })],
		["a listener that answers with no proof", () => new Response("{}", { status: 200 })],
		["a listener that answers a non-string proof", () => new Response('{"proof":42}', { status: 200 })],
		["a listener that answers garbage", () => new Response("not json", { status: 200 })],
		["a listener that replays another nonce's proof", () => new Response(JSON.stringify({ proof: probeProof(KEY, "some-other-nonce-0123456789", 41234, 900) }), { status: 200 })],
	])("does not verify %s", async (_name, answer) => {
		const check = createKeyPossessionCheck({ key: KEY, fetchImpl: (async () => answer()) as never, newNonce: () => NONCE });
		check.ensure(ORIGIN, 900);
		await settle();
		expect(check.isVerified(ORIGIN, 900)).toBe(false);
	});

	it("does not verify a listener that relays the nonce to the real daemon (the proof is for the real port)", async () => {
		const realOrigin = "http://127.0.0.1:41000";
		const real = daemon(KEY);
		// The forged listener sits on ORIGIN, forwards the probe to the real daemon and returns its genuine answer.
		const relay = vi.fn(async (_url: string | URL | Request, init?: RequestInit) => real(`${realOrigin}/internal/human-capability/probe`, init));
		const check = createKeyPossessionCheck({ key: KEY, fetchImpl: relay as never, newNonce: () => NONCE });
		check.ensure(ORIGIN, 900);
		await settle();
		expect(relay).toHaveBeenCalledTimes(1);
		expect(check.isVerified(ORIGIN, 900)).toBe(false);
		// The same genuine answer is accepted where it belongs.
		check.ensure(realOrigin, 900);
		const direct = createKeyPossessionCheck({ key: KEY, fetchImpl: real as never, newNonce: () => NONCE });
		direct.ensure(realOrigin, 900);
		await settle();
		expect(direct.isVerified(realOrigin, 900)).toBe(true);
	});

	it("does not verify a daemon that answers as some other process than the one the app spawned", async () => {
		const check = createKeyPossessionCheck({ key: KEY, fetchImpl: daemon(KEY, 200, 4242) as never, newNonce: () => NONCE });
		check.ensure(ORIGIN, 900);
		await settle();
		expect(check.isVerified(ORIGIN, 900)).toBe(false);
	});

	it("starts no probe for an origin without a port or for no pid", async () => {
		const fetchImpl = daemon(KEY);
		const check = createKeyPossessionCheck({ key: KEY, fetchImpl: fetchImpl as never });
		check.ensure("http://127.0.0.1", 900);
		check.ensure("not a url", 900);
		check.ensure(ORIGIN, undefined);
		check.ensure(ORIGIN, 0);
		await settle();
		expect(fetchImpl).not.toHaveBeenCalled();
	});

	it("does not verify when the request fails or hangs", async () => {
		const check = createKeyPossessionCheck({
			key: KEY,
			fetchImpl: (async () => {
				throw new Error("ECONNREFUSED");
			}) as never,
			newNonce: () => NONCE,
		});
		check.ensure(ORIGIN, 900);
		await settle();
		expect(check.isVerified(ORIGIN, 900)).toBe(false);
	});

	it("verifies per origin and per daemon pid, so a restarted daemon or another port is checked again", async () => {
		const fetchImpl = daemon(KEY);
		const check = createKeyPossessionCheck({ key: KEY, fetchImpl: fetchImpl as never });
		check.ensure(ORIGIN, 900);
		await settle();
		expect(check.isVerified(ORIGIN, 900)).toBe(true);
		expect(check.isVerified(ORIGIN, 901)).toBe(false);
		expect(check.isVerified("http://127.0.0.1:41235", 900)).toBe(false);
	});

	it("uses a fresh nonce for every probe", async () => {
		const fetchImpl = daemon(KEY);
		const check = createKeyPossessionCheck({ key: KEY, fetchImpl: fetchImpl as never });
		check.ensure(ORIGIN, 900);
		check.ensure("http://127.0.0.1:41235", 900);
		await settle();
		const nonces = fetchImpl.mock.calls.map((call) => (JSON.parse(String(call[1]?.body)) as { nonce: string }).nonce);
		expect(new Set(nonces).size).toBe(2);
		expect(nonces.every((nonce) => nonce.length >= 24)).toBe(true);
	});

	it("does not start a second probe while one is pending or after one succeeded", async () => {
		const fetchImpl = daemon(KEY);
		const check = createKeyPossessionCheck({ key: KEY, fetchImpl: fetchImpl as never });
		check.ensure(ORIGIN, 900);
		check.ensure(ORIGIN, 900);
		await settle();
		check.ensure(ORIGIN, 900);
		await settle();
		expect(fetchImpl).toHaveBeenCalledTimes(1);
	});

	it("retries a failed check only after a pause, and gives up after a few attempts", async () => {
		let clock = 1_000_000;
		let calls = 0;
		const check = createKeyPossessionCheck({
			key: KEY,
			fetchImpl: (async () => {
				calls += 1;
				return new Response("{}", { status: 404 });
			}) as never,
			now: () => clock,
		});
		check.ensure(ORIGIN, 900);
		await settle();
		check.ensure(ORIGIN, 900); // too soon
		await settle();
		expect(calls).toBe(1);
		for (let i = 0; i < 10; i += 1) {
			clock += 1500;
			check.ensure(ORIGIN, 900);
			await settle();
		}
		expect(calls).toBe(5);
		expect(check.isVerified(ORIGIN, 900)).toBe(false);
	});

	it("a daemon that was not ready at first can still verify on a later retry", async () => {
		let clock = 1_000_000;
		let up = false;
		const answering = daemon(KEY);
		const check = createKeyPossessionCheck({
			key: KEY,
			fetchImpl: (async (url: string | URL | Request, init?: RequestInit) => (up ? answering(url, init) : new Response("{}", { status: 503 }))) as never,
			now: () => clock,
		});
		check.ensure(ORIGIN, 900);
		await settle();
		up = true;
		clock += 1500;
		check.ensure(ORIGIN, 900);
		await settle();
		expect(check.isVerified(ORIGIN, 900)).toBe(true);
	});
});
