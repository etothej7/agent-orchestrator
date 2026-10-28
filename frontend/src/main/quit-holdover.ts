import type { SpawnOptions } from "node:child_process";

/**
 * Quitting AO withdraws its Stay Awake lease. With the lid shut, macOS then sleeps the Mac at
 * once: the display goes dark and the lock screen follows, even for a quick restart or an update.
 * (A `caffeinate` assertion does not stop this: it holds the system awake but not the lid-closed
 * policy, which only the wake helper's own override overrides, and only while a live lease asks.)
 *
 * So a quit that goes ahead starts a detached `ao wake-hold`. It owns a lease of its own, outlives
 * the app, and lets go when AO is reopened (see backend/internal/cli/wake_hold.go).
 */
export const WAKE_HOLD_ARGS = ["wake-hold"];

/** The wake helper rewrites its status every few seconds; an older file means it is not running. */
export const WAKE_STATUS_MAX_AGE_SECONDS = 60;

/** The part of the wake helper's status file (<data dir>/wake/status.json) a quit reads. */
export interface WakeStatusForHoldOver {
	active?: boolean;
	lidActive?: boolean;
	/** Seconds since the epoch, written by frontend/native/wake-helper/main.swift. */
	updatedAt?: number;
}

/** Stay Awake is holding a closed-lid Mac right now, on macOS, according to a fresh status file. */
export function shouldHoldOverQuit(status: WakeStatusForHoldOver | undefined, nowSeconds: number, platform: NodeJS.Platform): boolean {
	if (platform !== "darwin" || !status) return false;
	if (status.active !== true || status.lidActive !== true) return false;
	return typeof status.updatedAt === "number" && nowSeconds - status.updatedAt <= WAKE_STATUS_MAX_AGE_SECONDS;
}

interface SpawnedHoldOver {
	unref(): void;
	on(event: "error", listener: (error: Error) => void): unknown;
}

export interface QuitHoldOverDeps {
	platform: NodeJS.Platform;
	/** Resolved at quit time, not at start-up. */
	statusPath(): string;
	readStatus(file: string): string;
	nowSeconds(): number;
	/** The `ao` binary that runs `wake-hold`, or undefined when there is none this can run directly. */
	command(): string | undefined;
	/** The environment the daemon runs in: data dir, run file and wake helper. */
	env(): NodeJS.ProcessEnv;
	spawn(command: string, args: string[], options: SpawnOptions): SpawnedHoldOver;
	log(message: string, error?: unknown): void;
}

function parseStatus(raw: string): WakeStatusForHoldOver | undefined {
	const parsed: unknown = JSON.parse(raw);
	return parsed !== null && typeof parsed === "object" && !Array.isArray(parsed) ? (parsed as WakeStatusForHoldOver) : undefined;
}

/**
 * Starts the hold-over when this quit would otherwise let a closed-lid Mac sleep. Returns
 * whether one was started. It never throws: nothing here may delay or break a quit.
 */
export function startQuitHoldOver(deps: QuitHoldOverDeps): boolean {
	let status: WakeStatusForHoldOver | undefined;
	try {
		status = parseStatus(deps.readStatus(deps.statusPath()));
	} catch {
		return false;
	}
	if (!shouldHoldOverQuit(status, deps.nowSeconds(), deps.platform)) return false;
	try {
		const command = deps.command();
		if (!command) return false;
		const child = deps.spawn(command, WAKE_HOLD_ARGS, { detached: true, stdio: "ignore", env: deps.env() });
		child.on("error", (error) => deps.log("quit hold-over ended early", error));
		child.unref();
		return true;
	} catch (error) {
		deps.log("quit hold-over could not start", error);
		return false;
	}
}

/** One hold-over per quit: `before-quit` runs again when a held quit is retried. */
export function createQuitHoldOver(deps: QuitHoldOverDeps): { start(): boolean } {
	let started = false;
	return {
		start(): boolean {
			if (!started) started = startQuitHoldOver(deps);
			return started;
		},
	};
}
