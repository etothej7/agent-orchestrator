import { randomUUID } from "node:crypto";
import { link, mkdir, readFile, rm, writeFile } from "node:fs/promises";
import path from "node:path";

// The result of one unattended update, as the runner leaves it for the update manager (and, from ed.8, for the
// daemon's fallback notice). One small JSON file, write-once, 0600, no hostnames, no addresses, no credentials:
// `<stateDir>/update/runs/<stamp>/result.json`. See frontend/docs/unattended-update.md.

export type UpdateOutcome = "ok" | "rolled_back" | "stopped" | "deferred";
export type UpdateCheckResult = "ok" | "fail" | "skip";

export type UpdateResult = {
	schemaVersion: 1;
	stamp: string;
	outcome: UpdateOutcome;
	fromVersion: string;
	toVersion: string;
	/** One short message in plain words, for the person. */
	text: string;
	checks: Array<{ name: string; result: UpdateCheckResult }>;
	at: number;
};

export class UpdateResultError extends Error {
	constructor(readonly code: "invalid" | "sensitive" | "invalid_stamp" | "exists", message: string) {
		super(message);
		this.name = "UpdateResultError";
	}
}

const STAMP = /^[A-Za-z0-9._-]{6,64}$/;
const VERSION = /^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$/;
const OUTCOMES: readonly UpdateOutcome[] = ["ok", "rolled_back", "stopped", "deferred"];
const CHECK_RESULTS: readonly UpdateCheckResult[] = ["ok", "fail", "skip"];
const MAX_TEXT = 600;
const MAX_CHECKS = 40;
const MAX_CHECK_NAME = 60;
// eslint-disable-next-line no-control-regex
const CONTROL = /[\u0000-\u0008\u000b\u000c\u000e-\u001f\u007f]/g;

// A result is read by people and by other agents, and may be forwarded. It must never carry a place to reach
// Ed's Mac or a secret: URLs, IPv4 addresses, tailnet and tunnel host names, and token-shaped strings.
const SENSITIVE: readonly RegExp[] = [
	/https?:\/\//i,
	/\b\d{1,3}(?:\.\d{1,3}){3}\b/,
	/\.ts\.net\b/i,
	/trycloudflare/i,
	/\bsk-[A-Za-z0-9_-]{16,}/,
	/\bgh[pousr]_[A-Za-z0-9]{20,}/,
	/\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}/,
	/\b[a-f0-9]{32,}\b/i,
];

function assertNotSensitive(text: string): void {
	if (SENSITIVE.some((pattern) => pattern.test(text))) {
		throw new UpdateResultError("sensitive", "sensitive content in an update result: it may not carry an address, host name or credential");
	}
}

const invalid = (what: string): never => {
	throw new UpdateResultError("invalid", `invalid update result: ${what}`);
};

/** Validates and normalizes a result. Throws UpdateResultError; never returns a partly valid value. */
export function validateUpdateResult(value: unknown): UpdateResult {
	if (typeof value !== "object" || value === null || Array.isArray(value)) return invalid("not an object");
	const v = value as Record<string, unknown>;
	if (v.schemaVersion !== 1) invalid("schemaVersion");
	if (typeof v.stamp !== "string" || !STAMP.test(v.stamp)) invalid("stamp");
	if (typeof v.outcome !== "string" || !OUTCOMES.includes(v.outcome as UpdateOutcome)) invalid("outcome");
	if (typeof v.fromVersion !== "string" || !VERSION.test(v.fromVersion)) invalid("fromVersion");
	if (typeof v.toVersion !== "string" || !VERSION.test(v.toVersion)) invalid("toVersion");
	if (typeof v.text !== "string") invalid("text");
	const text = (v.text as string).replace(CONTROL, "").trim();
	if (text === "" || text.length > MAX_TEXT) invalid("text length");
	assertNotSensitive(text);
	if (!Array.isArray(v.checks) || v.checks.length > MAX_CHECKS) return invalid("checks");
	const checks = (v.checks as unknown[]).map((entry) => {
		if (typeof entry !== "object" || entry === null) return invalid("check");
		const c = entry as Record<string, unknown>;
		if (typeof c.name !== "string" || c.name.trim() === "") return invalid("check name");
		assertNotSensitive(c.name);
		if (c.name.length > MAX_CHECK_NAME) return invalid("check name length");
		if (typeof c.result !== "string" || !CHECK_RESULTS.includes(c.result as UpdateCheckResult)) return invalid("check result");
		return { name: c.name, result: c.result as UpdateCheckResult };
	});
	if (typeof v.at !== "number" || !Number.isFinite(v.at)) invalid("at");
	return {
		schemaVersion: 1,
		stamp: v.stamp as string,
		outcome: v.outcome as UpdateOutcome,
		fromVersion: v.fromVersion as string,
		toVersion: v.toVersion as string,
		text,
		checks,
		at: v.at as number,
	};
}

/** The result path for a stamp. The stamp cannot name anything outside the runs directory. */
export function updateResultFile(stateDir: string, stamp: string): string {
	if (!STAMP.test(stamp) || stamp === "." || stamp === "..") throw new UpdateResultError("invalid_stamp", "invalid update stamp");
	return path.join(stateDir, "update", "runs", stamp, "result.json");
}

/**
 * Writes the result once. The same result again is accepted (a retry), a different result for the same stamp is
 * refused, and nothing is written for an invalid or sensitive one. Returns the file path.
 */
export async function writeUpdateResult(stateDir: string, input: unknown): Promise<string> {
	const result = validateUpdateResult(input);
	const file = updateResultFile(stateDir, result.stamp);
	const body = JSON.stringify(result);
	await mkdir(path.dirname(file), { recursive: true, mode: 0o700 });
	const temporary = `${file}.${randomUUID()}.tmp`;
	await writeFile(temporary, body, { mode: 0o600 });
	try {
		await link(temporary, file); // atomic and exclusive: EEXIST means a result is already there
	} catch (error) {
		if ((error as NodeJS.ErrnoException).code !== "EEXIST") throw error;
		const existing = await readFile(file, "utf8").catch(() => "");
		if (existing !== body) throw new UpdateResultError("exists", "a different result is already recorded for this update");
	} finally {
		await rm(temporary, { force: true }).catch(() => undefined);
	}
	return file;
}

/** Reads a result back; undefined for a missing, corrupt, tampered or invalid one. */
export async function readUpdateResult(stateDir: string, stamp: string): Promise<UpdateResult | undefined> {
	try {
		const raw = await readFile(updateResultFile(stateDir, stamp), "utf8");
		return validateUpdateResult(JSON.parse(raw));
	} catch {
		return undefined;
	}
}
