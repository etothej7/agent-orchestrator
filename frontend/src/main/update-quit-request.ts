import { randomUUID } from "node:crypto";
import { lstat, mkdir, readFile, rename, rm, writeFile } from "node:fs/promises";
import path from "node:path";

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
//  * unsent drafts defer the quit, they never open a dialog; the request stays until it expires so the quit
//    happens as soon as the drafts are gone;
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
const MAX_REQUEST_BYTES = 4096;
const DEFAULT_POLL_MS = 1_000;

export type UpdateQuitRequest = {
	stamp: string;
	/** The version being installed (informational). */
	version: string;
	/** The pid of the app that must quit. */
	appPid: number;
	runnerPid: number;
	issuedAt: number;
	expiresAt: number;
};

export type QuitRequestRejection = "invalid" | "expired" | "not_yet_valid" | "lifetime" | "unsafe_file" | "wrong_target";

export type UpdateQuitStatus = {
	state: "quitting" | "deferred" | "rejected" | "expired";
	/** A fixed enum, never raw error text. */
	reason?: QuitRequestRejection | "unsent_drafts";
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
		request: { stamp: v.stamp, version: v.version, appPid: v.appPid, runnerPid: v.runnerPid, issuedAt: v.issuedAt, expiresAt: v.expiresAt },
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
	const discard = async () => {
		await rm(file, { force: true }).catch(() => undefined);
		quitAskedAt = undefined;
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

			if (deps.draftRiskCount() > 0) {
				quitAskedAt = undefined;
				await report({ state: "deferred", reason: "unsent_drafts", stamp: request.stamp });
				return;
			}
			if (quitAskedAt && quitAskedAt.stamp === request.stamp && deps.now() - quitAskedAt.at < QUIT_RETRY_AFTER_MS) return;
			quitAskedAt = { stamp: request.stamp, at: deps.now() };
			const outcome = deps.quitForUpdate();
			if (outcome === "deferred") {
				quitAskedAt = undefined;
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
