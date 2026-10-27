// @vitest-environment node
import { readdirSync, readFileSync, statSync } from "node:fs";
import path from "node:path";
import { describe, expect, it } from "vitest";
import { safeStorageUsesStandInKey, shouldUseMockKeychain } from "./keychain-policy";

const srcDir = path.resolve(__dirname, "..");

describe("shouldUseMockKeychain", () => {
	const cases: Array<[string, { isPackaged: boolean; version: string; platform: NodeJS.Platform }, boolean]> = [
		["a dev run on macOS", { isPackaged: false, version: "0.14.5", platform: "darwin" }, true],
		["a dev run of a personal version on macOS", { isPackaged: false, version: "0.14.5-ed.7.2", platform: "darwin" }, true],
		["a packaged personal build on macOS (ed.7.2)", { isPackaged: true, version: "0.14.5-ed.7.2", platform: "darwin" }, true],
		["a packaged personal build on macOS (ed.8)", { isPackaged: true, version: "0.15.0-ed.8", platform: "darwin" }, true],
		["a packaged personal build with no sub-number", { isPackaged: true, version: "0.14.5-ed", platform: "darwin" }, true],
		["a packaged stock release keeps the real keychain", { isPackaged: true, version: "0.14.5", platform: "darwin" }, false],
		["a packaged stock prerelease keeps the real keychain", { isPackaged: true, version: "0.14.5-beta.2", platform: "darwin" }, false],
		["a packaged nightly keeps the real keychain", { isPackaged: true, version: "0.14.5-nightly.20261027", platform: "darwin" }, false],
		["a version that only mentions ed is not personal", { isPackaged: true, version: "0.14.5-fed.1", platform: "darwin" }, false],
		["an unreadable version keeps the real keychain", { isPackaged: true, version: "", platform: "darwin" }, false],
		["a packaged personal build on Windows (the switch is macOS only)", { isPackaged: true, version: "0.14.5-ed.7.2", platform: "win32" }, false],
		["a dev run on Linux", { isPackaged: false, version: "0.14.5", platform: "linux" }, false],
	];
	for (const [name, input, expected] of cases) {
		it(`${expected ? "uses" : "does not use"} the stand-in key for ${name}`, () => {
			expect(shouldUseMockKeychain(input)).toBe(expected);
		});
	}
});

describe("safeStorageUsesStandInKey", () => {
	it("is true only for a shipped build that runs on the stand-in key (a packaged personal build on macOS)", () => {
		expect(safeStorageUsesStandInKey({ isPackaged: true, version: "0.14.5-ed.7.2", platform: "darwin" })).toBe(true);
		expect(safeStorageUsesStandInKey({ isPackaged: true, version: "0.14.5", platform: "darwin" })).toBe(false);
		expect(safeStorageUsesStandInKey({ isPackaged: true, version: "0.14.5-ed.7.2", platform: "linux" })).toBe(false);
		expect(safeStorageUsesStandInKey({ isPackaged: true, version: "0.14.5-ed.7.2", platform: "win32" })).toBe(false);
		// an unpackaged dev run also uses the stand-in key but is not a shipped build: its behaviour is unchanged
		expect(safeStorageUsesStandInKey({ isPackaged: false, version: "0.14.5", platform: "darwin" })).toBe(false);
	});
});

describe("how main applies it", () => {
	const main = readFileSync(path.join(srcDir, "main.ts"), "utf8");

	it("refuses browser cookie import under exactly the same policy (history still imports)", () => {
		expect(main).toMatch(/cookieImportRefused:\s*shouldUseMockKeychain\(\{\s*isPackaged: app\.isPackaged,\s*version: app\.getVersion\(\),\s*platform: process\.platform\s*\}\)/);
	});

	it("sets the switch from the policy, before the app is ready", () => {
		const use = main.indexOf("shouldUseMockKeychain({");
		const sw = main.indexOf('appendSwitch("use-mock-keychain")');
		const ready = main.indexOf("app.whenReady()");
		expect(use).toBeGreaterThan(0);
		expect(sw).toBeGreaterThan(use);
		expect(ready).toBeGreaterThan(sw);
		// no other place turns the switch on or off
		expect(main.match(/use-mock-keychain/g)?.length).toBe(1);
	});
});

describe("the existing 'Orchestrator.inc Safe Storage' keychain item is never touched by AO's desktop code", () => {
	const files: string[] = [];
	const walk = (dir: string) => {
		for (const name of readdirSync(dir)) {
			const full = path.join(dir, name);
			if (statSync(full).isDirectory()) {
				if (name !== "node_modules") walk(full);
			} else if (/\.(ts|tsx)$/.test(name) && !/\.(test|spec)\.tsx?$/.test(name)) {
				files.push(full);
			}
		}
	};
	walk(srcDir);

	it("scans a realistic number of source files", () => {
		expect(files.length).toBeGreaterThan(100);
	});

	const stripComments = (text: string) => text.replace(/\/\*[\s\S]*?\*\//g, "").replace(/(^|[^:])\/\/.*$/gm, "$1");
	// The user-initiated browser profile import reads OTHER browsers' keys (see the next test); nothing else may.
	const importer = path.join("main", "browser-profile-import.ts");

	it("names the item, and runs the macOS keychain command line tool, nowhere (comments aside)", () => {
		const offenders: string[] = [];
		const forbidden = [/Safe Storage/, /find-generic-password/, /add-generic-password/, /delete-generic-password/, /delete-keychain/, /dump-keychain/, /set-generic-password-partition-list/, /\bsecurity\s+(?:find|add|delete|dump|set)-/];
		for (const file of files) {
			if (path.relative(srcDir, file) === importer) continue;
			const text = stripComments(readFileSync(file, "utf8"));
			for (const pattern of forbidden) if (pattern.test(text)) offenders.push(`${path.relative(srcDir, file)}: ${pattern}`);
		}
		expect(offenders).toEqual([]);
	});

	it("the browser profile import can only ask for other browsers' keys, never AO's own", () => {
		const text = readFileSync(path.join(srcDir, importer), "utf8");
		const names = [...text.matchAll(/chromiumKeychainNames:\s*\[([^\]]*)\]/g)].flatMap((m) => [...m[1].matchAll(/"([^"]+)"/g)].map((n) => n[1]));
		expect(names.length).toBeGreaterThan(3);
		for (const name of names) {
			expect(name, name).not.toMatch(/orchestrator|\bao\b|dev\.agent/i);
		}
		// the only keychain command in that file reads a key named after one of those browsers
		expect(text.match(/find-generic-password/g)?.length).toBe(1);
		expect(text).not.toMatch(/add-generic-password|delete-generic-password|set-generic-password-partition-list/);
	});
});
