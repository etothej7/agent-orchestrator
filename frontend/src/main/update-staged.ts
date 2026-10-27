import { execFile } from "node:child_process";
import { lstat, readFile, realpath } from "node:fs/promises";
import path from "node:path";

// A quit request must name a staged update, not just ask for a quit. The runner stages the new app under
// `<stateDir>/update/staged/<stamp>/` and the request carries its path and version. This turns "one JSON file" into
// "stage a whole app" and stops stale or accidental requests outright. It is not authentication (a same-user process
// can fabricate a staged folder): the cryptographic checks of the package (CDHash, hashes) stay with the runner.
// See frontend/docs/unattended-update.md.

const MAX_PLIST_BYTES = 1_000_000;

export type StagedRequest = { stamp: string; version: string; stagedApp: string };

export type StagedCheck = { ok: true; app: string } | { ok: false };

type Owned = { isDirectory(): boolean; isFile(): boolean; uid: number; mode: number; size: number };

const privateEnough = (info: Owned, uid: number): boolean => info.uid === uid && (info.mode & 0o022) === 0;

/** CFBundleShortVersionString of an Info.plist (XML, or binary through plutil). Undefined when there is none. */
export async function readAppVersion(file: string): Promise<string | undefined> {
	const raw = await readFile(file);
	if (raw.subarray(0, 6).toString("latin1") === "bplist") {
		return await new Promise<string | undefined>((resolve) => {
			execFile("/usr/bin/plutil", ["-extract", "CFBundleShortVersionString", "raw", "-o", "-", file], { timeout: 5_000 }, (error, stdout) => {
				resolve(error ? undefined : String(stdout).trim() || undefined);
			});
		});
	}
	const match = /<key>\s*CFBundleShortVersionString\s*<\/key>\s*<string>([^<]*)<\/string>/.exec(raw.toString("utf8"));
	return match?.[1]?.trim() || undefined;
}

/**
 * True when the request's staged app is a real, user-owned, not group/world-writable app inside
 * `<stateDir>/update/staged/<stamp>/` (realpath, so a symlink cannot lead out) whose Info.plist version is the
 * version the request names. Never throws.
 */
export async function verifyStagedPackage(input: {
	stateDir: string;
	uid: number;
	request: StagedRequest;
	readAppVersion?: (file: string) => Promise<string | undefined>;
}): Promise<StagedCheck> {
	const { stateDir, uid, request } = input;
	try {
		const stampDir = path.join(stateDir, "update", "staged", request.stamp);
		const stampInfo = await lstat(stampDir);
		if (!stampInfo.isDirectory() || !privateEnough(stampInfo, uid)) return { ok: false };
		const realState = await realpath(stateDir);
		const realStamp = await realpath(stampDir);
		if (realStamp !== path.join(realState, "update", "staged", request.stamp)) return { ok: false };

		const realApp = await realpath(request.stagedApp);
		if (!realApp.startsWith(realStamp + path.sep) || !realApp.endsWith(".app")) return { ok: false };
		const appInfo = await lstat(realApp);
		if (!appInfo.isDirectory() || !privateEnough(appInfo, uid)) return { ok: false };

		// realpath again: a symlinked Contents/ (or Info.plist) must not lead out of the staged app.
		const plistFile = await realpath(path.join(realApp, "Contents", "Info.plist"));
		if (!plistFile.startsWith(realApp + path.sep)) return { ok: false };
		const plistInfo = await lstat(plistFile);
		if (!plistInfo.isFile() || plistInfo.size > MAX_PLIST_BYTES || !privateEnough(plistInfo, uid)) return { ok: false };
		const version = await (input.readAppVersion ?? readAppVersion)(plistFile);
		if (version !== request.version) return { ok: false };
		return { ok: true, app: realApp };
	} catch {
		return { ok: false };
	}
}
