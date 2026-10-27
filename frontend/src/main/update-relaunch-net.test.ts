// @vitest-environment node
import { spawn as realSpawn } from "node:child_process";
import { existsSync } from "node:fs";
import { chmod, mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { EventEmitter } from "node:events";
import os from "node:os";
import path from "node:path";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { RELAUNCH_NET_DELAY_SECONDS, createRelaunchNet, relaunchNetDir } from "./update-relaunch-net";

const STAMP = "20261027T230000Z";
const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));

describe("the relaunch safety net", () => {
	let root: string;
	let state: string;
	let opened: string; // the fake `open` appends its arguments here
	let running: string; // the fake `pgrep` succeeds (an AO app is running) while this file exists
	let lock: string;
	let marker: string;
	const dir = () => relaunchNetDir(state, STAMP);
	const outcome = async () => (await readFile(path.join(dir(), "outcome"), "utf8").catch(() => "")).trim();
	const openedCalls = async () => (await readFile(opened, "utf8").catch(() => "")).trim().split("\n").filter(Boolean);
	const waitFor = async (check: () => Promise<boolean> | boolean, ms = 8_000) => {
		const end = Date.now() + ms;
		while (Date.now() < end) {
			if (await check()) return;
			await sleep(25);
		}
		throw new Error("condition not reached in time");
	};

	const make = (over: Partial<Parameters<typeof createRelaunchNet>[0]> = {}) =>
		createRelaunchNet({
			stateDir: state,
			appPath: "/Applications/Agent Orchestrator.app",
			exeName: "agent-orchestrator",
			lockPath: lock,
			markerPath: marker,
			delaySeconds: 0.2,
			stepSeconds: 0.05,
			lockWaitSeconds: 1,
			openCommand: path.join(root, "fake-open"),
			pgrepCommand: path.join(root, "fake-pgrep"),
			...over,
		});

	beforeEach(async () => {
		root = await mkdtemp(path.join(os.tmpdir(), "ao-net-"));
		state = path.join(root, "state");
		await mkdir(state, { recursive: true });
		opened = path.join(root, "opened.log");
		running = path.join(root, "app-running");
		lock = path.join(state, "install.lock");
		marker = path.join(state, "install-swap-marker");
		await writeFile(path.join(root, "fake-open"), `#!/bin/sh\necho "$@" >> '${opened}'\n`);
		await writeFile(path.join(root, "fake-pgrep"), `#!/bin/sh\n[ -e '${running}' ]\n`);
		await chmod(path.join(root, "fake-open"), 0o755);
		await chmod(path.join(root, "fake-pgrep"), 0o755);
	});
	afterEach(async () => {
		// stop any net still polling
		await writeFile(path.join(dir(), "cancel"), "").catch(() => undefined);
		await sleep(150);
		await rm(root, { recursive: true, force: true });
	});

	it("waits about ten minutes by default", () => {
		expect(RELAUNCH_NET_DELAY_SECONDS).toBe(600);
	});

	it("arms a detached process that records itself, and arming twice reuses it (idempotent)", async () => {
		const net = make({ delaySeconds: 30, stepSeconds: 0.1 });
		expect(await net.arm(STAMP)).toBe(true);
		const pid = Number((await readFile(path.join(dir(), "net.pid"), "utf8")).trim());
		expect(pid).toBeGreaterThan(1);
		expect(() => process.kill(pid, 0)).not.toThrow();
		expect(await net.arm(STAMP)).toBe(true);
		expect(Number((await readFile(path.join(dir(), "net.pid"), "utf8")).trim())).toBe(pid);
		await net.cancel(STAMP);
		await waitFor(async () => (await outcome()) === "cancelled");
		expect(await openedCalls()).toEqual([]);
	});

	it("FIRES: after the delay, with AO not running and no install under way, it relaunches hidden", async () => {
		const net = make();
		expect(await net.arm(STAMP)).toBe(true);
		await waitFor(async () => (await outcome()) === "relaunched");
		await waitFor(async () => (await openedCalls()).length === 1);
		expect(await openedCalls()).toEqual(["-g -j /Applications/Agent Orchestrator.app"]);
	});

	it("is CANCELLED by the runner (a cancel file) before it fires: nothing is relaunched", async () => {
		const net = make({ delaySeconds: 1 });
		expect(await net.arm(STAMP)).toBe(true);
		await net.cancel(STAMP);
		await waitFor(async () => (await outcome()) === "cancelled");
		await sleep(1_200);
		expect(await openedCalls()).toEqual([]);
	});

	it("does nothing when AO is running again by the time it fires", async () => {
		await writeFile(running, "");
		const net = make();
		expect(await net.arm(STAMP)).toBe(true);
		await waitFor(async () => (await outcome()) === "app_running");
		expect(await openedCalls()).toEqual([]);
	});

	it("waits while the installer holds its lock, then relaunches once the lock is gone", async () => {
		await mkdir(lock);
		await writeFile(path.join(lock, "pid"), String(process.pid)); // a live owner
		const net = make({ lockWaitSeconds: 30 });
		expect(await net.arm(STAMP)).toBe(true);
		await sleep(600);
		expect(await outcome()).toBe("");
		expect(await openedCalls()).toEqual([]);
		await rm(lock, { recursive: true });
		await waitFor(async () => (await outcome()) === "relaunched");
	});

	it("waits while a swap or rollback marker exists, then relaunches once it is gone", async () => {
		await writeFile(marker, 'M_PHASE="swapped"\n');
		const net = make({ lockWaitSeconds: 30 });
		expect(await net.arm(STAMP)).toBe(true);
		await sleep(600);
		expect(await outcome()).toBe("");
		await rm(marker);
		await waitFor(async () => (await outcome()) === "relaunched");
	});

	it("ignores a stale lock whose owner is gone", async () => {
		await mkdir(lock);
		const dead = realSpawn("/usr/bin/true");
		await new Promise((resolve) => dead.on("exit", resolve));
		await writeFile(path.join(lock, "pid"), String(dead.pid));
		const net = make();
		expect(await net.arm(STAMP)).toBe(true);
		await waitFor(async () => (await outcome()) === "relaunched");
	});

	it("gives up waiting for a stuck install after the lock wait and relaunches anyway (AO never stays closed)", async () => {
		await writeFile(marker, "stuck\n");
		const net = make({ lockWaitSeconds: 0.4 });
		expect(await net.arm(STAMP)).toBe(true);
		await waitFor(async () => (await outcome()) === "relaunched");
		await waitFor(async () => (await openedCalls()).length === 1);
	});

	it("can be armed again after it was cancelled", async () => {
		const net = make({ delaySeconds: 30, stepSeconds: 0.1 });
		await net.arm(STAMP);
		await net.cancel(STAMP);
		await waitFor(async () => (await outcome()) === "cancelled");
		expect(await net.arm(STAMP)).toBe(true);
		expect(await outcome()).toBe("");
		expect(existsSync(path.join(dir(), "cancel"))).toBe(false);
		await net.cancel(STAMP);
	});

	it("reports failure (so the quit is not honoured) when it cannot arm: no state folder, or the process cannot start", async () => {
		await writeFile(path.join(root, "afile"), "x");
		expect(await make({ stateDir: path.join(root, "afile", "inside") }).arm(STAMP)).toBe(false);
		const failing = make({
			spawnNet: () => {
				const child = new EventEmitter() as ReturnType<typeof realSpawn>;
				(child as unknown as { unref: () => void }).unref = () => undefined;
				queueMicrotask(() => child.emit("error", new Error("spawn failed")));
				return child;
			},
		});
		expect(await failing.arm(STAMP)).toBe(false);
	});

	it("runs with no inherited secrets in its environment", async () => {
		const seen = path.join(root, "env.log");
		await writeFile(path.join(root, "fake-open"), `#!/bin/sh\nenv > '${seen}'\n`);
		process.env.AO_TEST_SECRET_TOKEN = "super-secret-value";
		try {
			await make().arm(STAMP);
			await waitFor(async () => (await outcome()) === "relaunched");
			await waitFor(() => existsSync(seen));
			expect(await readFile(seen, "utf8")).not.toContain("super-secret-value");
		} finally {
			delete process.env.AO_TEST_SECRET_TOKEN;
		}
	});

	it("cancel does nothing, and creates nothing, for a stamp that was never armed", async () => {
		await make().cancel("20990101T000000Z");
		expect(existsSync(relaunchNetDir(state, "20990101T000000Z"))).toBe(false);
	});
});
