import { readFileSync } from "node:fs";
import path from "node:path";
import { describe, expect, it, vi } from "vitest";
import {
	createQuitHoldOver,
	shouldHoldOverQuit,
	startQuitHoldOver,
	WAKE_HOLD_ARGS,
	WAKE_STATUS_MAX_AGE_SECONDS,
	type QuitHoldOverDeps,
} from "./quit-holdover";

// The wake helper's real status file, captured from ~/.ao/data/wake/status.json on 2026-10-27
// while Stay Awake held a closed-lid Mac (writer: frontend/native/wake-helper/main.swift).
const REAL_STATUS = '{"updatedAt":1793142164.1867719,"lidActive":true,"version":5,"blocked":{},"leases":["16adda90-89a8-481c-be19-c68e00dbf261"],"reason":"","active":true,"effectsBlocked":{}}';
const REAL_STATUS_TIME = 1793142164.1867719;
const AO = "/Applications/Agent Orchestrator.app/Contents/Resources/daemon/ao";
const ENV = { AO_DATA_DIR: "/Users/ed/.ao/data", AO_WAKE_HELPER: "/Applications/Agent Orchestrator.app/Contents/Resources/wake-helper/ao-wake" };

function fakeChild() {
	const handlers: Record<string, (error: Error) => void> = {};
	return {
		unref: vi.fn(),
		on: vi.fn((event: string, handler: (error: Error) => void) => { handlers[event] = handler; }),
		emit: (event: string, error: Error) => handlers[event]?.(error),
	};
}

function deps(overrides: Partial<QuitHoldOverDeps> = {}) {
	const child = fakeChild();
	const spawn = vi.fn(() => child);
	const log = vi.fn();
	const value: QuitHoldOverDeps = {
		platform: "darwin",
		statusPath: () => "/data/wake/status.json",
		readStatus: () => REAL_STATUS,
		nowSeconds: () => REAL_STATUS_TIME + 2,
		command: () => AO,
		env: () => ENV,
		spawn,
		log,
		...overrides,
	};
	return { value, spawn, log, child };
}

describe("shouldHoldOverQuit", () => {
	const fresh = { active: true, lidActive: true, updatedAt: 1000 };

	it("holds only on macOS, while Stay Awake is armed for a closed lid, with a fresh status", () => {
		expect(shouldHoldOverQuit(fresh, 1005, "darwin")).toBe(true);
		expect(shouldHoldOverQuit(fresh, 1005, "linux")).toBe(false);
		expect(shouldHoldOverQuit(fresh, 1005, "win32")).toBe(false);
		expect(shouldHoldOverQuit({ ...fresh, active: false }, 1005, "darwin")).toBe(false);
		expect(shouldHoldOverQuit({ ...fresh, lidActive: false }, 1005, "darwin")).toBe(false);
		expect(shouldHoldOverQuit({ active: true, lidActive: true }, 1005, "darwin")).toBe(false);
		expect(shouldHoldOverQuit(undefined, 1005, "darwin")).toBe(false);
	});

	it("ignores a status file the helper has stopped refreshing", () => {
		expect(shouldHoldOverQuit(fresh, 1000 + WAKE_STATUS_MAX_AGE_SECONDS, "darwin")).toBe(true);
		expect(shouldHoldOverQuit(fresh, 1000 + WAKE_STATUS_MAX_AGE_SECONDS + 1, "darwin")).toBe(false);
	});
});

describe("startQuitHoldOver", () => {
	it("hands Stay Awake to a detached `ao wake-hold` that outlives the app", () => {
		const { value, spawn, child } = deps();
		expect(startQuitHoldOver(value)).toBe(true);
		expect(spawn).toHaveBeenCalledTimes(1);
		expect(spawn).toHaveBeenCalledWith(AO, ["wake-hold"], { detached: true, stdio: "ignore", env: ENV });
		expect(child.unref).toHaveBeenCalledTimes(1);
	});

	it("runs the subcommand the Go CLI registers", () => {
		// backend/internal/cli/wake_hold.go: Use "wake-hold", hidden, no positional arguments.
		expect(WAKE_HOLD_ARGS).toEqual(["wake-hold"]);
	});

	it("does nothing when Stay Awake is not holding a closed-lid Mac", () => {
		const idle = '{"updatedAt":1793142164.18,"lidActive":false,"version":5,"blocked":{},"leases":[],"reason":"","active":false,"effectsBlocked":{}}';
		const { value, spawn } = deps({ readStatus: () => idle });
		expect(startQuitHoldOver(value)).toBe(false);
		expect(spawn).not.toHaveBeenCalled();
	});

	it("does nothing off macOS", () => {
		const { value, spawn } = deps({ platform: "linux" });
		expect(startQuitHoldOver(value)).toBe(false);
		expect(spawn).not.toHaveBeenCalled();
	});

	it("does nothing when there is no ao binary it can run directly", () => {
		const { value, spawn } = deps({ command: () => undefined });
		expect(startQuitHoldOver(value)).toBe(false);
		expect(spawn).not.toHaveBeenCalled();
	});

	it("never blocks or breaks a quit when the status file is missing, unreadable or garbage", () => {
		for (const readStatus of [() => { throw new Error("ENOENT"); }, () => "", () => "not json", () => "null", () => "[]"]) {
			const { value, spawn } = deps({ readStatus });
			expect(() => startQuitHoldOver(value)).not.toThrow();
			expect(startQuitHoldOver(value)).toBe(false);
			expect(spawn).not.toHaveBeenCalled();
		}
	});

	it("never breaks a quit when the hold cannot be started", () => {
		const { value, log } = deps({ spawn: () => { throw new Error("spawn EACCES"); } });
		expect(() => startQuitHoldOver(value)).not.toThrow();
		expect(startQuitHoldOver(value)).toBe(false);
		expect(log).toHaveBeenCalled();
		const { value: noCommand } = deps({ command: () => { throw new Error("no launch spec"); } });
		expect(() => startQuitHoldOver(noCommand)).not.toThrow();
	});

	it("logs, without throwing, when the spawned process reports an error later", () => {
		const { value, log, child } = deps();
		startQuitHoldOver(value);
		expect(() => child.emit("error", new Error("spawn ENOENT"))).not.toThrow();
		expect(log).toHaveBeenCalledWith(expect.stringContaining("hold-over"), expect.any(Error));
	});
});

describe("createQuitHoldOver", () => {
	it("starts one hold-over even though a quit passes before-quit more than once", () => {
		const { value, spawn } = deps();
		const holdOver = createQuitHoldOver(value);
		holdOver.start();
		holdOver.start();
		holdOver.start();
		expect(spawn).toHaveBeenCalledTimes(1);
	});

	it("tries again on a later quit if an earlier attempt did not apply", () => {
		let status = '{"updatedAt":1793142164.18,"lidActive":false,"active":false}';
		const { value, spawn } = deps({ readStatus: () => status });
		const holdOver = createQuitHoldOver(value);
		holdOver.start();
		expect(spawn).not.toHaveBeenCalled();
		status = REAL_STATUS;
		holdOver.start();
		expect(spawn).toHaveBeenCalledTimes(1);
	});

	it("resolves the status path, binary and environment when a quit happens, not when the app starts", () => {
		const statusPath = vi.fn(() => "/data/wake/status.json");
		const command = vi.fn(() => AO);
		const env = vi.fn(() => ENV);
		const holdOver = createQuitHoldOver(deps({ statusPath, command, env }).value);
		expect(statusPath).not.toHaveBeenCalled();
		expect(command).not.toHaveBeenCalled();
		expect(env).not.toHaveBeenCalled();
		holdOver.start();
		expect(statusPath).toHaveBeenCalledTimes(1);
		expect(command).toHaveBeenCalledTimes(1);
		expect(env).toHaveBeenCalledTimes(1);
	});
});

// main.ts is too large to boot in a test, so guard the wiring that makes the hold-over cover every quit.
describe("quit hold-over wiring in main.ts", () => {
	const main = readFileSync(path.resolve(process.cwd(), "src/main.ts"), "utf8");

	it("starts in before-quit only once the quit can no longer be held back, before any teardown", () => {
		const handler = main.slice(main.indexOf('app.on("before-quit"'));
		// The last thing that can still call the quit off: the unsent-draft confirmation.
		const gate = handler.indexOf("desktopQuit.cancelQuit()");
		const start = handler.indexOf("quitHoldOver.start()");
		const teardown = handler.indexOf("browserQuitRequested = true");
		expect(gate).toBeGreaterThan(-1);
		expect(start).toBeGreaterThan(gate);
		expect(start).toBeLessThan(teardown);
	});

	it("reads the wake helper's status from the daemon's data directory", () => {
		expect(main).toContain('path.join(resolvedDaemonDataDir(), "wake", "status.json")');
	});

	it("runs the same ao binary, data dir, run file and wake helper the daemon uses", () => {
		const block = main.slice(main.indexOf("const quitHoldOver = createQuitHoldOver("), main.indexOf("const desktopQuit = createDesktopQuitController("));
		expect(block).toContain("resolveDaemonLaunch(");
		expect(block).toContain('launch.source !== "configured"');
		expect(block).toContain("AO_DATA_DIR: resolvedDaemonDataDir()");
		expect(block).toContain("AO_WAKE_HELPER: bundledWakeHelperPath()");
		expect(block).toContain("AO_RUN_FILE");
		// The daemon's own environment takes its wake helper from the same function.
		expect(main.match(/AO_WAKE_HELPER: bundledWakeHelperPath\(\)/g)).toHaveLength(2);
	});
});
