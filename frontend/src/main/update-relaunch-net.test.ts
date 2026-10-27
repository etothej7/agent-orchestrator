// @vitest-environment node
import { spawn as realSpawn } from "node:child_process";
import { existsSync, statSync } from "node:fs";
import { chmod, mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { EventEmitter } from "node:events";
import os from "node:os";
import path from "node:path";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { RELAUNCH_NET_DELAY_SECONDS, appProcessPattern, createRelaunchNet, relaunchNetDir } from "./update-relaunch-net";
import { validateUpdateResult } from "./update-result";

const STAMP = "20261027T230000Z";
const REQUEST = { stamp: STAMP, version: "0.14.5-ed.7.2" };
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
			fromVersion: "0.14.5-ed.7.1",
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
		expect(await net.arm(REQUEST)).toBe(true);
		const pid = Number((await readFile(path.join(dir(), "net.pid"), "utf8")).trim());
		expect(pid).toBeGreaterThan(1);
		expect(() => process.kill(pid, 0)).not.toThrow();
		expect(await net.arm(REQUEST)).toBe(true);
		expect(Number((await readFile(path.join(dir(), "net.pid"), "utf8")).trim())).toBe(pid);
		await net.cancel(STAMP);
		await waitFor(async () => (await outcome()) === "cancelled");
		expect(await openedCalls()).toEqual([]);
	});

	it("FIRES: after the delay, with AO not running and no install under way, it relaunches hidden", async () => {
		const net = make();
		expect(await net.arm(REQUEST)).toBe(true);
		await waitFor(async () => (await outcome()) === "relaunched");
		await waitFor(async () => (await openedCalls()).length === 1);
		expect(await openedCalls()).toEqual(["-g -j /Applications/Agent Orchestrator.app"]);
	});

	it("is CANCELLED by the runner (a cancel file) before it fires: nothing is relaunched", async () => {
		const net = make({ delaySeconds: 1 });
		expect(await net.arm(REQUEST)).toBe(true);
		await net.cancel(STAMP);
		await waitFor(async () => (await outcome()) === "cancelled");
		await sleep(1_200);
		expect(await openedCalls()).toEqual([]);
	});

	it("does nothing when AO is running again by the time it fires", async () => {
		await writeFile(running, "");
		const net = make();
		expect(await net.arm(REQUEST)).toBe(true);
		await waitFor(async () => (await outcome()) === "app_running");
		expect(await openedCalls()).toEqual([]);
	});

	it("waits while the installer holds its lock, then relaunches once the lock is gone", async () => {
		await mkdir(lock);
		await writeFile(path.join(lock, "pid"), String(process.pid)); // a live owner
		const net = make({ lockWaitSeconds: 30 });
		expect(await net.arm(REQUEST)).toBe(true);
		await sleep(600);
		expect(await outcome()).toBe("");
		expect(await openedCalls()).toEqual([]);
		await rm(lock, { recursive: true });
		await waitFor(async () => (await outcome()) === "relaunched");
	});

	it("waits while a swap or rollback marker exists, then relaunches once it is gone", async () => {
		await writeFile(marker, 'M_PHASE="swapped"\n');
		const net = make({ lockWaitSeconds: 30 });
		expect(await net.arm(REQUEST)).toBe(true);
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
		expect(await net.arm(REQUEST)).toBe(true);
		await waitFor(async () => (await outcome()) === "relaunched");
	});

	it("gives up waiting for a live lock (no swap marker) after the lock wait and relaunches anyway (AO never stays closed)", async () => {
		await mkdir(lock);
		await writeFile(path.join(lock, "pid"), String(process.pid)); // a live owner that never finishes
		const net = make({ lockWaitSeconds: 0.4 });
		expect(await net.arm(REQUEST)).toBe(true);
		await waitFor(async () => (await outcome()) === "relaunched");
		await waitFor(async () => (await openedCalls()).length === 1);
	});

	it("can be armed again after it was cancelled", async () => {
		const net = make({ delaySeconds: 30, stepSeconds: 0.1 });
		await net.arm(REQUEST);
		await net.cancel(STAMP);
		await waitFor(async () => (await outcome()) === "cancelled");
		expect(await net.arm(REQUEST)).toBe(true);
		expect(await outcome()).toBe("");
		expect(existsSync(path.join(dir(), "cancel"))).toBe(false);
		await net.cancel(STAMP);
	});

	it("reports failure (so the quit is not honoured) when it cannot arm: no state folder, or the process cannot start", async () => {
		await writeFile(path.join(root, "afile"), "x");
		expect(await make({ stateDir: path.join(root, "afile", "inside") }).arm(REQUEST)).toBe(false);
		const failing = make({
			spawnNet: () => {
				const child = new EventEmitter() as ReturnType<typeof realSpawn>;
				(child as unknown as { unref: () => void }).unref = () => undefined;
				queueMicrotask(() => child.emit("error", new Error("spawn failed")));
				return child;
			},
		});
		expect(await failing.arm(REQUEST)).toBe(false);
	});

	it("runs with no inherited secrets in its environment", async () => {
		const seen = path.join(root, "env.log");
		await writeFile(path.join(root, "fake-open"), `#!/bin/sh\nenv > '${seen}'\n`);
		process.env.AO_TEST_SECRET_TOKEN = "super-secret-value";
		try {
			await make().arm(REQUEST);
			await waitFor(async () => (await outcome()) === "relaunched");
			await waitFor(() => existsSync(seen));
			expect(await readFile(seen, "utf8")).not.toContain("super-secret-value");
		} finally {
			delete process.env.AO_TEST_SECRET_TOKEN;
		}
	});

	it("B4: a net.pid whose number now belongs to an unrelated process is not a net: arm starts a real one", async () => {
		const dirPath = dir();
		await mkdir(dirPath, { recursive: true });
		await writeFile(path.join(dirPath, "net.pid"), String(process.pid)); // alive, but this is vitest, not a net
		const net = make({ delaySeconds: 30, stepSeconds: 0.1 });
		expect(await net.arm(REQUEST)).toBe(true);
		const pid = Number((await readFile(path.join(dirPath, "net.pid"), "utf8")).trim());
		expect(pid).not.toBe(process.pid);
		const { execFileSync } = await import("node:child_process");
		expect(execFileSync("/bin/ps", ["-ww", "-p", String(pid), "-o", "command="], { encoding: "utf8" })).toContain(" ao-relaunch-net ");
		await net.cancel(STAMP);
	});

	it("B4: a live net for this folder is reused (the real ps check recognises it)", async () => {
		const net = make({ delaySeconds: 30, stepSeconds: 0.1 });
		await net.arm(REQUEST);
		const pid = (await readFile(path.join(dir(), "net.pid"), "utf8")).trim();
		await net.arm(REQUEST);
		expect((await readFile(path.join(dir(), "net.pid"), "utf8")).trim()).toBe(pid);
		await net.cancel(STAMP);
	});

	it("F6: the lock's pid is digits only: a sign, a space or text is a stale lock, not a live owner (-1 must not hold the net)", async () => {
		for (const text of ["-1", " 1", "12 3", "abc", "1;2", "+5"]) {
			await rm(dir(), { recursive: true, force: true });
			await rm(lock, { recursive: true, force: true });
			await mkdir(lock);
			await writeFile(path.join(lock, "pid"), text);
			const net = make({ lockWaitSeconds: 30 });
			expect(await net.arm(REQUEST), text).toBe(true);
			await waitFor(async () => (await outcome()) === "relaunched", 4_000);
		}
	});

	it("B5: the pgrep pattern is the app path with regex characters escaped, anchored on the main executable folder", async () => {
		const app = "/Applications/Agent (Orchestrator) +1.app";
		const log = path.join(root, "pgrep.log");
		await writeFile(path.join(root, "fake-pgrep"), `#!/bin/sh\necho "$@" >> '${log}'\nexit 1\n`);
		const net = make({ appPath: app });
		await net.arm(REQUEST);
		await waitFor(async () => (await outcome()) === "relaunched");
		const pattern = appProcessPattern(app);
		expect(pattern).toBe("^/Applications/Agent \\(Orchestrator\\) \\+1\\.app/Contents/MacOS/");
		expect((await readFile(log, "utf8")).trim()).toBe(`-f ${pattern}`);
		// as a POSIX extended regex it matches exactly that path and nothing else
		expect(new RegExp(pattern).test(`${app}/Contents/MacOS/agent-orchestrator`)).toBe(true);
		expect(new RegExp(pattern).test("/Applications/AgentXOrchestratorX 1.app/Contents/MacOS/agent-orchestrator")).toBe(false);
	});

	describe("a swap marker still present when the wait runs out (a possibly half-swapped bundle)", () => {
		const resultFile = () => path.join(state, "update", "runs", STAMP, "result.json");
		const makeRecovery = async (name: string, exitCode: number, mode = 0o755) => {
			const file = path.join(root, name);
			await writeFile(file, `#!/bin/sh\necho recovered >> '${path.join(root, "recovery.log")}'\nexit ${exitCode}\n`);
			await chmod(file, mode);
			return file;
		};
		const recoveryCalls = async () => (await readFile(path.join(root, "recovery.log"), "utf8").catch(() => "")).trim().split("\n").filter(Boolean);
		beforeEach(async () => {
			await writeFile(marker, 'M_PHASE="swapped"\n');
		});

		it("never relaunches: it runs the recovery entry named in the request, which restores and relaunches itself", async () => {
			const recovery = await makeRecovery("recover.sh", 0);
			const net = make({ lockWaitSeconds: 0.2 });
			await net.arm({ ...REQUEST, recoveryEntry: recovery });
			await waitFor(async () => (await outcome()) === "recovery_ok");
			expect(await recoveryCalls()).toEqual(["recovered"]);
			expect(await openedCalls()).toEqual([]);
			expect(existsSync(resultFile())).toBe(false); // the installer's recovery reports for itself
		});

		it("takes the recovery entry from the marker (M_RECOVERY) when the request names none", async () => {
			const recovery = await makeRecovery("from-marker.sh", 0);
			await writeFile(marker, `M_PHASE="swapped"\nM_RECOVERY="${recovery}"\nM_STAMP="x"\n`);
			const net = make({ lockWaitSeconds: 0.2 });
			await net.arm(REQUEST);
			await waitFor(async () => (await outcome()) === "recovery_ok");
			expect(await recoveryCalls()).toEqual(["recovered"]);
			expect(await openedCalls()).toEqual([]);
		});

		it("with no recovery available it leaves AO closed (blocked_marker) and writes a valid stopped result for the update manager", async () => {
			const net = make({ lockWaitSeconds: 0.2 });
			await net.arm(REQUEST);
			await waitFor(async () => (await outcome()) === "blocked_marker");
			await sleep(200);
			expect(await openedCalls()).toEqual([]);
			const written = JSON.parse(await readFile(resultFile(), "utf8"));
			const result = validateUpdateResult(written); // the same validator the runner's writer uses
			expect(result).toMatchObject({
				stamp: STAMP, outcome: "stopped", fromVersion: "0.14.5-ed.7.1", toVersion: "0.14.5-ed.7.2",
				checks: [{ name: "relaunch_net_blocked_marker", result: "fail" }],
			});
			expect(result.text).toMatch(/left closed/);
			expect(statSync(resultFile()).mode & 0o777).toBe(0o600);
		});

		it("treats an unusable recovery entry (not executable, group-writable, relative, missing) as no recovery", async () => {
			for (const entry of [await makeRecovery("noexec.sh", 0, 0o644), await makeRecovery("gw.sh", 0, 0o775), "relative.sh", path.join(root, "missing.sh")]) {
				await rm(dir(), { recursive: true, force: true });
				await rm(path.dirname(resultFile()), { recursive: true, force: true });
				const net = make({ lockWaitSeconds: 0.2 });
				await net.arm({ ...REQUEST, recoveryEntry: entry });
				await waitFor(async () => (await outcome()) === "blocked_marker");
			}
			expect(await recoveryCalls()).toEqual([]);
			expect(await openedCalls()).toEqual([]);
		});

		it("a recovery that fails leaves AO closed and says so in the result", async () => {
			const recovery = await makeRecovery("fails.sh", 3);
			const net = make({ lockWaitSeconds: 0.2 });
			await net.arm({ ...REQUEST, recoveryEntry: recovery });
			await waitFor(async () => (await outcome()) === "recovery_failed");
			expect(await openedCalls()).toEqual([]);
			expect(validateUpdateResult(JSON.parse(await readFile(resultFile(), "utf8"))).checks).toEqual([{ name: "relaunch_net_recovery_failed", result: "fail" }]);
		});

		it("never overwrites a result the runner already wrote", async () => {
			await mkdir(path.dirname(resultFile()), { recursive: true });
			await writeFile(resultFile(), "{\"runner\":true}");
			const net = make({ lockWaitSeconds: 0.2 });
			await net.arm(REQUEST);
			await waitFor(async () => (await outcome()) === "blocked_marker");
			expect(await readFile(resultFile(), "utf8")).toBe("{\"runner\":true}");
		});

		it("relaunches normally when the marker goes away before the wait runs out", async () => {
			const net = make({ lockWaitSeconds: 30 });
			await net.arm(REQUEST);
			await sleep(500);
			await rm(marker);
			await waitFor(async () => (await outcome()) === "relaunched");
			expect(await recoveryCalls()).toEqual([]);
		});
	});

	it("cancel does nothing, and creates nothing, for a stamp that was never armed", async () => {
		await make().cancel("20990101T000000Z");
		expect(existsSync(relaunchNetDir(state, "20990101T000000Z"))).toBe(false);
	});
});
