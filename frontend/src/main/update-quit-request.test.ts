// @vitest-environment node
import { chmod, mkdir, mkdtemp, readFile, rm, stat, symlink, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
	MAX_QUIT_REQUEST_LIFETIME_MS,
	QUIT_RETRY_AFTER_MS,
	createUpdateQuitWatcher,
	parseUpdateQuitRequest,
	quitRequestFile,
	quitStatusFile,
} from "./update-quit-request";

const NOW = 1_800_000_000_000;
const APP_PID = 4242;
const uid = process.getuid?.() ?? 0;

const goodRequest = (over: Record<string, unknown> = {}) => ({
	stamp: "20261027T230000Z",
	version: "0.14.5-ed.7.2",
	appPid: APP_PID,
	runnerPid: 9001,
	issuedAt: NOW - 1_000,
	expiresAt: NOW + 5 * 60_000,
	...over,
});

describe("parseUpdateQuitRequest", () => {
	const parse = (value: unknown, now = NOW) => parseUpdateQuitRequest(JSON.stringify(value), now);

	it("accepts a well-formed, unexpired request", () => {
		const result = parse(goodRequest());
		expect(result).toEqual({ ok: true, request: goodRequest() });
	});

	it("rejects text that is not a JSON object, without throwing", () => {
		for (const raw of ["", "not json", "null", "[]", "42", '"x"', "{"]) {
			expect(parseUpdateQuitRequest(raw, NOW)).toEqual({ ok: false, reason: "invalid" });
		}
	});

	it("rejects missing, mistyped or malformed fields", () => {
		for (const over of [
			{ stamp: undefined }, { stamp: "short" }, { stamp: "has space in it ok?" }, { stamp: "x".repeat(65) },
			{ version: "latest" }, { version: "" }, { version: 7 },
			{ appPid: 0 }, { appPid: -3 }, { appPid: 1.5 }, { appPid: "4242" },
			{ runnerPid: 0 }, { runnerPid: null },
			{ issuedAt: "now" }, { expiresAt: Number.NaN }, { expiresAt: undefined },
		]) {
			expect(parse(goodRequest(over)), JSON.stringify(over)).toEqual({ ok: false, reason: "invalid" });
		}
	});

	it("rejects an expired request, one that expires before it was issued, and one that lives too long", () => {
		expect(parse(goodRequest({ expiresAt: NOW }))).toMatchObject({ ok: false, reason: "expired" });
		expect(parse(goodRequest({ expiresAt: NOW - 1 }))).toMatchObject({ ok: false, reason: "expired" });
		expect(parse(goodRequest({ issuedAt: NOW - 10, expiresAt: NOW - 20 }))).toEqual({ ok: false, reason: "invalid" });
		expect(parse(goodRequest({ expiresAt: NOW + MAX_QUIT_REQUEST_LIFETIME_MS + 5_000 }))).toMatchObject({ ok: false, reason: "lifetime" });
	});

	it("rejects a request issued in the future beyond the clock-skew allowance", () => {
		expect(parse(goodRequest({ issuedAt: NOW + 10 * 60_000, expiresAt: NOW + 12 * 60_000 }))).toMatchObject({ ok: false, reason: "not_yet_valid" });
		// a small skew is fine
		expect(parse(goodRequest({ issuedAt: NOW + 5_000 })).ok).toBe(true);
	});

	it("ignores unknown extra fields rather than failing (the runner may add notes)", () => {
		expect(parse(goodRequest({ note: "x" })).ok).toBe(true);
	});
});

describe("the watcher", () => {
	let stateDir: string;
	let clock: { value: number };
	let drafts: { value: number };
	let quits: number;
	let quitAnswer: "quitting" | "deferred";
	let logs: string[];

	const dir = () => path.join(stateDir, "update-restart");
	const writeRequest = async (value: unknown, mode = 0o600) => {
		await mkdir(dir(), { recursive: true, mode: 0o700 });
		const file = quitRequestFile(stateDir);
		await writeFile(file, typeof value === "string" ? value : JSON.stringify(value), { mode });
		await chmod(file, mode);
	};
	const status = async () => {
		try {
			return JSON.parse(await readFile(quitStatusFile(stateDir), "utf8"));
		} catch {
			return undefined;
		}
	};
	const exists = async (file: string) => stat(file).then(() => true, () => false);
	const make = (over: Partial<Parameters<typeof createUpdateQuitWatcher>[0]> = {}) =>
		createUpdateQuitWatcher({
			stateDir, pid: APP_PID, uid, now: () => clock.value,
			draftRiskCount: () => drafts.value,
			quitForUpdate: () => { quits += 1; return quitAnswer; },
			log: (message) => logs.push(message),
			...over,
		});

	beforeEach(async () => {
		stateDir = await mkdtemp(path.join(os.tmpdir(), "ao-update-quit-"));
		clock = { value: NOW };
		drafts = { value: 0 };
		quits = 0;
		quitAnswer = "quitting";
		logs = [];
	});
	afterEach(async () => {
		await rm(stateDir, { recursive: true, force: true });
	});

	it("does nothing, and creates nothing, when there is no request", async () => {
		const watcher = make();
		await watcher.tick();
		expect(quits).toBe(0);
		expect(await exists(dir())).toBe(false);
	});

	it("quits once for a valid request addressed to this process and says so", async () => {
		await writeRequest(goodRequest());
		const watcher = make();
		await watcher.tick();
		expect(quits).toBe(1);
		expect(await status()).toMatchObject({ state: "quitting", stamp: "20261027T230000Z", pid: APP_PID });
		// the request is kept until this app exits: a vetoed quit must be retryable
		expect(await exists(quitRequestFile(stateDir))).toBe(true);
	});

	it("is idempotent: further ticks while the quit is under way do not quit again", async () => {
		await writeRequest(goodRequest());
		const watcher = make();
		await watcher.tick();
		clock.value += 2_000;
		await watcher.tick();
		await watcher.tick();
		expect(quits).toBe(1);
	});

	it("retries a quit that did not take effect (the app is still here) after the retry delay", async () => {
		await writeRequest(goodRequest());
		const watcher = make();
		await watcher.tick();
		clock.value += QUIT_RETRY_AFTER_MS + 1;
		await watcher.tick();
		expect(quits).toBe(2);
	});

	it("never asks about drafts: with unsent drafts it defers, keeps the request and quits once they are gone", async () => {
		await writeRequest(goodRequest());
		drafts.value = 2;
		const watcher = make();
		await watcher.tick();
		await watcher.tick();
		expect(quits).toBe(0);
		expect(await status()).toMatchObject({ state: "deferred", reason: "unsent_drafts" });
		expect(await exists(quitRequestFile(stateDir))).toBe(true);
		drafts.value = 0;
		clock.value += 1_000;
		await watcher.tick();
		expect(quits).toBe(1);
		expect(await status()).toMatchObject({ state: "quitting" });
	});

	it("reports a deferral from the controller (a draft appeared at the last moment) and tries again later", async () => {
		await writeRequest(goodRequest());
		quitAnswer = "deferred";
		const watcher = make();
		await watcher.tick();
		expect(quits).toBe(1);
		expect(await status()).toMatchObject({ state: "deferred", reason: "unsent_drafts" });
		quitAnswer = "quitting";
		clock.value += 1_000;
		await watcher.tick();
		expect(quits).toBe(2);
	});

	it("gives up on a deferred request once it expires, and says so", async () => {
		await writeRequest(goodRequest({ expiresAt: NOW + 3_000 }));
		drafts.value = 1;
		const watcher = make();
		await watcher.tick();
		clock.value += 10_000;
		await watcher.tick();
		expect(quits).toBe(0);
		expect(await status()).toMatchObject({ state: "expired", stamp: "20261027T230000Z" });
		expect(await exists(quitRequestFile(stateDir))).toBe(false);
	});

	it("ignores and removes a request addressed to another process (a stale file must never quit the new app)", async () => {
		await writeRequest(goodRequest({ appPid: APP_PID + 1 }));
		const watcher = make();
		await watcher.tick();
		expect(quits).toBe(0);
		expect(await status()).toMatchObject({ state: "rejected", reason: "wrong_target" });
		expect(await exists(quitRequestFile(stateDir))).toBe(false);
	});

	it("rejects and removes invalid, over-long or future-dated requests without quitting", async () => {
		const cases: Array<[unknown, string]> = [
			["{ nope", "invalid"],
			[goodRequest({ expiresAt: NOW + MAX_QUIT_REQUEST_LIFETIME_MS * 2 }), "lifetime"],
			[goodRequest({ issuedAt: NOW + 3_600_000, expiresAt: NOW + 3_700_000 }), "not_yet_valid"],
		];
		for (const [value, reason] of cases) {
			await writeRequest(value);
			await make().tick();
			expect(quits).toBe(0);
			expect(await status(), reason).toMatchObject({ state: "rejected", reason });
			expect(await exists(quitRequestFile(stateDir))).toBe(false);
		}
	});

	it("reports an already-expired request as expired and removes it", async () => {
		await writeRequest(goodRequest({ expiresAt: NOW - 1 }));
		await make().tick();
		expect(quits).toBe(0);
		expect(await status()).toMatchObject({ state: "expired" });
		expect(await exists(quitRequestFile(stateDir))).toBe(false);
	});

	it("rejects a request file that other users could have written: group/world writable, a symlink, or owned by someone else", async () => {
		await writeRequest(goodRequest(), 0o666);
		await make().tick();
		expect(await status()).toMatchObject({ state: "rejected", reason: "unsafe_file" });
		expect(quits).toBe(0);

		await rm(quitRequestFile(stateDir), { force: true });
		const target = path.join(stateDir, "elsewhere.json");
		await writeFile(target, JSON.stringify(goodRequest()), { mode: 0o600 });
		await symlink(target, quitRequestFile(stateDir));
		await make().tick();
		expect(await status()).toMatchObject({ state: "rejected", reason: "unsafe_file" });
		expect(quits).toBe(0);

		await rm(quitRequestFile(stateDir), { force: true });
		await writeRequest(goodRequest());
		await make({ uid: uid + 1 }).tick();
		expect(await status()).toMatchObject({ state: "rejected", reason: "unsafe_file" });
		expect(quits).toBe(0);
	});

	it("rejects an oversized request file", async () => {
		await writeRequest(JSON.stringify({ ...goodRequest(), pad: "x".repeat(8192) }));
		await make().tick();
		expect(await status()).toMatchObject({ state: "rejected", reason: "invalid" });
		expect(quits).toBe(0);
	});

	it("never overlaps two ticks", async () => {
		await writeRequest(goodRequest());
		const watcher = make();
		await Promise.all([watcher.tick(), watcher.tick(), watcher.tick()]);
		expect(quits).toBe(1);
	});

	it("keeps going when the status file cannot be written (a quit must not depend on it)", async () => {
		await writeRequest(goodRequest());
		await rm(dir(), { recursive: true, force: true });
		await mkdir(dir(), { recursive: true, mode: 0o700 });
		await writeRequest(goodRequest());
		// a directory where the status file should go makes the write fail
		await mkdir(quitStatusFile(stateDir), { recursive: true });
		await make().tick();
		expect(quits).toBe(1);
		expect(logs.some((line) => /status/i.test(line))).toBe(true);
	});

	it("start() polls until stop()", async () => {
		// Only the interval is faked: the file reads are real I/O.
		vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
		try {
			await writeRequest(goodRequest());
			const watcher = make({ intervalMs: 500 });
			watcher.start();
			await vi.advanceTimersByTimeAsync(600);
			await vi.waitFor(() => expect(quits).toBe(1));
			watcher.stop();
			clock.value += QUIT_RETRY_AFTER_MS + 1;
			await vi.advanceTimersByTimeAsync(5_000);
			await new Promise((resolve) => setTimeout(resolve, 50));
			expect(quits).toBe(1);
		} finally {
			vi.useRealTimers();
		}
	});
});
