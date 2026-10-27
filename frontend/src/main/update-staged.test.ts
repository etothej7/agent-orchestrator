// @vitest-environment node
import { chmod, mkdir, mkdtemp, rm, symlink, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { readAppVersion, verifyStagedPackage } from "./update-staged";

const uid = process.getuid?.() ?? 0;
const STAMP = "20261027T230000Z";
const VERSION = "0.14.5-ed.7.2";

const plist = (version: string) =>
	`<?xml version="1.0" encoding="UTF-8"?>\n<plist version="1.0"><dict><key>CFBundleName</key><string>Agent Orchestrator</string>` +
	`<key>CFBundleShortVersionString</key><string>${version}</string></dict></plist>`;

describe("verifyStagedPackage", () => {
	let state: string;
	const stampDir = () => path.join(state, "update", "staged", STAMP);
	const appPath = () => path.join(stampDir(), "Agent Orchestrator.app");
	const request = (over: Record<string, string> = {}) => ({ stamp: STAMP, version: VERSION, stagedApp: appPath(), ...over });
	const verify = (over: Record<string, string> = {}, extra: Partial<Parameters<typeof verifyStagedPackage>[0]> = {}) =>
		verifyStagedPackage({ stateDir: state, uid, request: request(over), ...extra });

	const stage = async (version = VERSION) => {
		await mkdir(path.join(appPath(), "Contents"), { recursive: true, mode: 0o700 });
		await writeFile(path.join(appPath(), "Contents", "Info.plist"), plist(version), { mode: 0o644 });
	};

	beforeEach(async () => {
		state = await mkdtemp(path.join(os.tmpdir(), "ao-staged-"));
		await mkdir(path.join(state, "update", "staged"), { recursive: true, mode: 0o700 });
	});
	afterEach(async () => {
		await rm(state, { recursive: true, force: true });
	});

	it("accepts a user-owned staged app inside update/staged/<stamp>/ whose version matches the request", async () => {
		await stage();
		const result = await verify();
		expect(result.ok).toBe(true);
		if (result.ok) expect(result.app.endsWith("Agent Orchestrator.app")).toBe(true);
	});

	it("refuses a path outside update/staged/<stamp>/ (another folder, another stamp, the staged root itself)", async () => {
		await stage();
		const elsewhere = path.join(state, "elsewhere", "Agent Orchestrator.app");
		await mkdir(path.join(elsewhere, "Contents"), { recursive: true });
		await writeFile(path.join(elsewhere, "Contents", "Info.plist"), plist(VERSION));
		expect((await verify({ stagedApp: elsewhere })).ok).toBe(false);
		expect((await verify({ stagedApp: path.join(state, "update", "staged") })).ok).toBe(false);
		expect((await verify({ stagedApp: path.join(stampDir(), "..", "other-stamp", "Agent Orchestrator.app") })).ok).toBe(false);
		expect((await verify({ stagedApp: stampDir() })).ok).toBe(false); // not an .app
	});

	it("refuses a staged app that does not exist", async () => {
		await mkdir(stampDir(), { recursive: true, mode: 0o700 });
		expect((await verify()).ok).toBe(false);
	});

	it("refuses a symlink that points out of the staged folder (realpath, not the lexical path)", async () => {
		await mkdir(stampDir(), { recursive: true, mode: 0o700 });
		const elsewhere = path.join(state, "elsewhere", "Agent Orchestrator.app");
		await mkdir(path.join(elsewhere, "Contents"), { recursive: true });
		await writeFile(path.join(elsewhere, "Contents", "Info.plist"), plist(VERSION));
		await symlink(elsewhere, appPath());
		expect((await verify()).ok).toBe(false);
	});

	it("refuses a symlinked stamp folder or staged root", async () => {
		const target = path.join(state, "elsewhere");
		await mkdir(path.join(target, "Agent Orchestrator.app", "Contents"), { recursive: true });
		await writeFile(path.join(target, "Agent Orchestrator.app", "Contents", "Info.plist"), plist(VERSION));
		await symlink(target, stampDir());
		expect((await verify()).ok).toBe(false);
		await rm(stampDir(), { force: true });
		await rm(path.join(state, "update", "staged"), { recursive: true, force: true });
		await symlink(target, path.join(state, "update", "staged"));
		expect((await verify({ stagedApp: path.join(target, "Agent Orchestrator.app") })).ok).toBe(false);
	});

	it("refuses folders or an app that group or others can write, or that another user owns", async () => {
		await stage();
		await chmod(appPath(), 0o777);
		expect((await verify()).ok).toBe(false);
		await chmod(appPath(), 0o755);
		expect((await verify()).ok).toBe(true);
		await chmod(stampDir(), 0o775);
		expect((await verify()).ok).toBe(false);
		await chmod(stampDir(), 0o700);
		expect((await verify({}, { uid: uid + 1 })).ok).toBe(false);
	});

	it("refuses an Info.plist that group or others can write", async () => {
		await stage();
		await chmod(path.join(appPath(), "Contents", "Info.plist"), 0o666);
		expect((await verify()).ok).toBe(false);
	});

	it("refuses when the app's version is not the version the request names", async () => {
		await stage("0.14.5-ed.7.1");
		expect((await verify()).ok).toBe(false);
		expect((await verify({ version: "0.14.5-ed.7.1" })).ok).toBe(true);
	});

	it("refuses a missing, unreadable or version-less Info.plist, and never throws", async () => {
		await mkdir(path.join(appPath(), "Contents"), { recursive: true, mode: 0o700 });
		expect((await verify()).ok).toBe(false);
		await writeFile(path.join(appPath(), "Contents", "Info.plist"), "<plist><dict></dict></plist>", { mode: 0o644 });
		expect((await verify()).ok).toBe(false);
		expect((await verify({}, { readAppVersion: async () => { throw new Error("boom"); } })).ok).toBe(false);
	});

	it("does not parse an oversized Info.plist", async () => {
		await stage();
		await writeFile(path.join(appPath(), "Contents", "Info.plist"), plist(VERSION) + " ".repeat(2_000_000), { mode: 0o644 });
		expect((await verify()).ok).toBe(false);
	});
});

describe("readAppVersion", () => {
	let dir: string;
	beforeEach(async () => {
		dir = await mkdtemp(path.join(os.tmpdir(), "ao-plist-"));
	});
	afterEach(async () => {
		await rm(dir, { recursive: true, force: true });
	});

	it("reads CFBundleShortVersionString from an XML property list, whatever the key order", async () => {
		const file = path.join(dir, "Info.plist");
		await writeFile(file, plist("1.2.3-ed.4"));
		expect(await readAppVersion(file)).toBe("1.2.3-ed.4");
		await writeFile(file, `<plist><dict><key>CFBundleShortVersionString</key>\n\t<string>9.9.9</string>\n<key>CFBundleVersion</key><string>1</string></dict></plist>`);
		expect(await readAppVersion(file)).toBe("9.9.9");
	});

	it("reads a binary property list through plutil (macOS)", async () => {
		if (process.platform !== "darwin") return;
		const file = path.join(dir, "Info.plist");
		await writeFile(file, plist("5.6.7-ed.8"));
		const { execFileSync } = await import("node:child_process");
		execFileSync("/usr/bin/plutil", ["-convert", "binary1", file]);
		expect(await readAppVersion(file)).toBe("5.6.7-ed.8");
	});

	it("returns undefined when there is no version", async () => {
		const file = path.join(dir, "Info.plist");
		await writeFile(file, "<plist><dict></dict></plist>");
		expect(await readAppVersion(file)).toBeUndefined();
	});
});
