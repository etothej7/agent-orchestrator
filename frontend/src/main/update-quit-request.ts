import { randomUUID } from "node:crypto";
import { lstat, mkdir, readFile, rename, rm, writeFile } from "node:fs/promises";
import path from "node:path";
import { verifyStagedPackage } from "./update-staged";

// A quit the update runner can ask for without a dialog and without an AppleEvent.
//
// The runner (outside AO) writes `<stateDir>/update-restart/quit-request.json`. This module validates it, and
// when it is addressed to THIS process and no unsent draft is at risk, asks the desktop-quit controller for an
// update quit (`quitForUpdate`, which never prompts). It reports what it did in `quit-status.json`. See
// frontend/docs/unattended-update.md for the contract.
//
// Safety properties:
//  * the request must name this process's pid, so a file left behind by an earlier run can never quit the
//    NEXT app (the relaunched build has another pid, rejects it and removes it);
//  * the file must be a regular file owned by this user and not group- or world-accessible, so another user or
//    a swapped-in symlink cannot trigger a quit;
//  * it expires (at most 30 minutes) and is bounded in size;
//  * it names a staged update (`stagedApp` + `version`): the app must be a real, user-owned, private app inside
//    `<stateDir>/update/staged/<stamp>/` whose Info.plist version is the version named, so one stray JSON file
//    cannot make AO quit (update-staged.ts);
//  * AO honours it only while the Mac has been idle for five minutes and no AO window is focused; otherwise it
//    defers (`user_active`), keeping the request alive like a draft deferral;
//  * unsent drafts defer the quit, they never open a dialog; the request stays until it expires so the quit
//    happens as soon as the drafts are gone;
//  * before it quits, AO arms the relaunch safety net (update-relaunch-net.ts): if the quit is honoured and nothing
//    brings AO back, a detached process relaunches it hidden after ten minutes. If the net cannot be armed, AO does
//    not quit;
//  * a same-user process could always SIGTERM the app, so this adds no privilege.

const REQUEST_FILE = "quit-request.json";
const STATUS_FILE = "quit-status.json";
const DIR = "update-restart";

/** A request may not live longer than this. */
export const MAX_QUIT_REQUEST_LIFETIME_MS = 30 * 60_000;
/** A request issued slightly in the future is accepted (clock skew between the runner and the app). */
export const QUIT_CLOCK_SKEW_MS = 60_000;
/** A quit that was asked for and did not take effect (the app is still here) is asked for again after this. */
export const QUIT_RETRY_AFTER_MS = 30_000;
/** The Mac must have been idle this long (seconds) before a request is honoured. */
export const UPDATE_QUIT_MIN_IDLE_SECONDS = 300;
const MAX_REQUEST_BYTES = 4096;
const MAX_STAGED_PATH = 1024;
const DEFAULT_POLL_MS = 1_000;

export type UpdateQuitRequest = {
	stamp: string;
	/** The version being installed; must equal the staged app's Info.plist version. */
	version: string;
	/** The staged app (a path inside `<stateDir>/update/staged/<stamp>/`). */
	stagedApp: string;
	/** The installer's recovery entry (an absolute path), run by the relaunch net if an install is left half-swapped. */
	recoveryEntry?: string;
	/** The pid of the app that must quit. */
	appPid: number;
	runnerPid: number;
	issuedAt: number;
	expiresAt: number;
};

export type QuitRequestRejection = "invalid" | "expired" | "not_yet_valid" | "lifetime" | "unsafe_file" | "wrong_target" | "staged_invalid";

export type UpdateQuitStatus = {
	state: "quitting" | "deferred" | "rejected" | "expired";
	/** A fixed enum, never raw error text. */
	reason?: QuitRequestRejection | "unsent_drafts" | "user_active" | "safety_net_unavailable";
	stamp?: string;
	pid: number;
	at: number;
};

const STAMP = /^[A-Za-z0-9._-]{6,64}$/;
const VERSION = /^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$/;

const positiveInt = (value: unknown): value is number => typeof value === "number" && Number.isInteger(value) && value > 0;

export const quitRequestFile = (stateDir: string): string => path.join(stateDir, DIR, REQUEST_FILE);
export const quitStatusFile = (stateDir: string): string => path.join(stateDir, DIR, STATUS_FILE);

export type ParsedQuitRequest =
	| { ok: true; request: UpdateQuitRequest }
	| { ok: false; reason: "invalid" }
	| { ok: false; reason: "expired" | "not_yet_valid" | "lifetime"; stamp: string };

/** Validates the request text. Never throws. */
export function parseUpdateQuitRequest(raw: string, now: number): ParsedQuitRequest {
	let value: unknown;
	try {
		value = JSON.parse(raw);
	} catch {
		return { ok: false, reason: "invalid" };
	}
	if (typeof value !== "object" || value === null || Array.isArray(value)) return { ok: false, reason: "invalid" };
	const v = value as Record<string, unknown>;
	if (
		typeof v.stamp !== "string" || !STAMP.test(v.stamp) ||
		typeof v.version !== "string" || !VERSION.test(v.version) ||
		typeof v.stagedApp !== "string" || v.stagedApp.length === 0 || v.stagedApp.length > MAX_STAGED_PATH || v.stagedApp.includes("\0") || !path.isAbsolute(v.stagedApp) ||
		(v.recoveryEntry !== undefined && (typeof v.recoveryEntry !== "string" || v.recoveryEntry.length === 0 || v.recoveryEntry.length > MAX_STAGED_PATH || v.recoveryEntry.includes("\0") || !path.isAbsolute(v.recoveryEntry))) ||
		!positiveInt(v.appPid) || !positiveInt(v.runnerPid) ||
		typeof v.issuedAt !== "number" || !Number.isFinite(v.issuedAt) ||
		typeof v.expiresAt !== "number" || !Number.isFinite(v.expiresAt) ||
		v.expiresAt <= v.issuedAt
	) return { ok: false, reason: "invalid" };
	if (v.expiresAt - v.issuedAt > MAX_QUIT_REQUEST_LIFETIME_MS) return { ok: false, reason: "lifetime", stamp: v.stamp };
	if (v.issuedAt > now + QUIT_CLOCK_SKEW_MS) return { ok: false, reason: "not_yet_valid", stamp: v.stamp };
	if (v.expiresAt <= now) return { ok: false, reason: "expired", stamp: v.stamp };
	return {
		ok: true,
		request: { stamp: v.stamp, version: v.version, stagedApp: v.stagedApp, ...(typeof v.recoveryEntry === "string" ? { recoveryEntry: v.recoveryEntry } : {}), appPid: v.appPid, runnerPid: v.runnerPid, issuedAt: v.issuedAt, expiresAt: v.expiresAt },
	};
}

async function writeStatus(stateDir: string, status: UpdateQuitStatus): Promise<void> {
	const dir = path.join(stateDir, DIR);
	await mkdir(dir, { recursive: true, mode: 0o700 });
	const file = quitStatusFile(stateDir);
	const temporary = `${file}.${randomUUID()}.tmp`;
	await writeFile(temporary, JSON.stringify(status), { mode: 0o600 });
	await rename(temporary, file).catch(async (error) => {
		await rm(temporary, { force: true }).catch(() => undefined);
		throw error;
	});
}

export type UpdateQuitWatcherDeps = {
	stateDir: string;
	/** This process's pid: the only pid a request may name. */
	pid: number;
	/** The user id that must own the request file. */
	uid: number;
	now(): number;
	/** Unsent drafts at risk right now. */
	draftRiskCount(): number;
	/** Seconds since the person last used the Mac (powerMonitor.getSystemIdleTime). */
	userIdleSeconds(): number;
	/** An AO window has the focus. */
	appWindowFocused(): boolean;
	/** The relaunch safety net: `arm` is true only once it is running; `cancel` stops it. */
	relaunchNet: {
		arm(request: { stamp: string; version: string; recoveryEntry?: string }): Promise<boolean>;
		cancel(stamp: string): Promise<void>;
	};
	/** Gives up an update quit that was asked for (clears its no-dialog state). */
	cancelUpdateQuit(): void;
	/** Idle seconds required; defaults to UPDATE_QUIT_MIN_IDLE_SECONDS. */
	minIdleSeconds?: number;
	/** Test seam for reading the staged app's version. */
	readAppVersion?(file: string): Promise<string | undefined>;
	/** Asks for the update quit. Must never prompt. "deferred" means a draft appeared and the quit was held. */
	quitForUpdate(): "quitting" | "deferred";
	log?(message: string, detail?: Record<string, unknown>): void;
	intervalMs?: number;
};

export function createUpdateQuitWatcher(deps: UpdateQuitWatcherDeps) {
	const log = deps.log ?? (() => undefined);
	const file = quitRequestFile(deps.stateDir);
	let busy = false;
	let timer: ReturnType<typeof setInterval> | undefined;
	let lastStatusKey = "";
	let quitAskedAt: { stamp: string; at: number } | undefined;
	let netArmedFor: string | undefined;

	const report = async (status: Omit<UpdateQuitStatus, "pid" | "at">) => {
		const key = JSON.stringify(status);
		if (key === lastStatusKey) return;
		try {
			await writeStatus(deps.stateDir, { ...status, pid: deps.pid, at: deps.now() });
			lastStatusKey = key;
		} catch {
			// The quit must not depend on the status file.
			log("update quit: could not write the status file");
		}
	};
	/** Nothing is quitting now: stop the safety net (it is armed only while a quit is in progress) and the no-dialog state. */
	const standDown = async () => {
		quitAskedAt = undefined;
		deps.cancelUpdateQuit();
		if (netArmedFor) {
			const stamp = netArmedFor;
			netArmedFor = undefined;
			await deps.relaunchNet.cancel(stamp).catch(() => undefined);
		}
	};
	const discard = async () => {
		await rm(file, { force: true }).catch(() => undefined);
		await standDown();
	};
	const reject = async (reason: QuitRequestRejection, stamp?: string) => {
		log("update quit: request rejected", { reason });
		await report({ state: reason === "expired" ? "expired" : "rejected", ...(reason === "expired" ? {} : { reason }), ...(stamp ? { stamp } : {}) });
		await discard();
	};

	async function tick(): Promise<void> {
		if (busy) return;
		busy = true;
		try {
			let info;
			try {
				info = await lstat(file);
			} catch {
				return; // no request: the normal case
			}
			if (!info.isFile() || info.uid !== deps.uid || (info.mode & 0o077) !== 0) return await reject("unsafe_file");
			if (info.size > MAX_REQUEST_BYTES) return await reject("invalid");
			let raw: string;
			try {
				raw = await readFile(file, "utf8");
			} catch {
				return await reject("invalid");
			}
			const parsed = parseUpdateQuitRequest(raw, deps.now());
			if (!parsed.ok) return await reject(parsed.reason, "stamp" in parsed ? parsed.stamp : undefined);
			const { request } = parsed;
			if (request.appPid !== deps.pid) return await reject("wrong_target", request.stamp);

			const staged = await verifyStagedPackage({
				stateDir: deps.stateDir,
				uid: deps.uid,
				request,
				...(deps.readAppVersion ? { readAppVersion: deps.readAppVersion } : {}),
			});
			if (!staged.ok) return await reject("staged_invalid", request.stamp);

			// The quit has been asked for and is under way (the app can take seconds to tear down). Nothing here can stop it
			// any more, so the gates are not asked again and the net is never cancelled: only a quit that was actually
			// prevented (a draft held it in before-quit, or this window passed with the app still alive) is stood down.
			if (quitAskedAt && quitAskedAt.stamp === request.stamp && deps.now() - quitAskedAt.at < QUIT_RETRY_AFTER_MS) return;

			if (deps.appWindowFocused() || deps.userIdleSeconds() < (deps.minIdleSeconds ?? UPDATE_QUIT_MIN_IDLE_SECONDS)) {
				await standDown();
				await report({ state: "deferred", reason: "user_active", stamp: request.stamp });
				return;
			}
			if (deps.draftRiskCount() > 0) {
				await standDown();
				await report({ state: "deferred", reason: "unsent_drafts", stamp: request.stamp });
				return;
			}
			// AO never quits without a recovery that does not depend on the runner staying alive.
			if (!(await deps.relaunchNet.arm({ stamp: request.stamp, version: request.version, ...(request.recoveryEntry ? { recoveryEntry: request.recoveryEntry } : {}) }))) {
				await report({ state: "deferred", reason: "safety_net_unavailable", stamp: request.stamp });
				return;
			}
			netArmedFor = request.stamp;
			quitAskedAt = { stamp: request.stamp, at: deps.now() };
			const outcome = deps.quitForUpdate();
			if (outcome === "deferred") {
				await standDown();
				await report({ state: "deferred", reason: "unsent_drafts", stamp: request.stamp });
				return;
			}
			await report({ state: "quitting", stamp: request.stamp });
		} finally {
			busy = false;
		}
	}

	return {
		tick,
		start(): void {
			if (timer) return;
			timer = setInterval(() => void tick().catch((error) => log("update quit: check failed", { error: String(error).slice(0, 120) })), deps.intervalMs ?? DEFAULT_POLL_MS);
			timer.unref?.();
		},
		stop(): void {
			if (timer) clearInterval(timer);
			timer = undefined;
		},
	};
}
