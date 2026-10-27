// @vitest-environment node
import { readFileSync } from "node:fs";
import path from "node:path";
import { describe, expect, it } from "vitest";
import { aoWindowFocused } from "./app-focus";

const none = { getFocusedWindow: () => null };
const some = { getFocusedWindow: () => ({}) };

describe("aoWindowFocused", () => {
	it("is true when the BaseWindow shell is focused, although BrowserWindow reports nothing (the real AO window)", () => {
		expect(aoWindowFocused({ BaseWindow: some, BrowserWindow: none })).toBe(true);
	});

	it("is true when a BrowserWindow is focused", () => {
		expect(aoWindowFocused({ BaseWindow: none, BrowserWindow: some })).toBe(true);
	});

	it("is false when no window is focused", () => {
		expect(aoWindowFocused({ BaseWindow: none, BrowserWindow: none })).toBe(false);
	});
});

describe("how main wires the update quit's focus gate", () => {
	const main = readFileSync(path.resolve(process.cwd(), "src/main.ts"), "utf8");
	const watcher = main.slice(main.indexOf("function startUpdateQuitWatcher"), main.indexOf("function resolveBundlePath"));

	it("asks the shell's real focus state, never BrowserWindow alone", () => {
		expect(watcher).toContain("aoWindowFocused({ BaseWindow, BrowserWindow })");
		expect(watcher).not.toMatch(/appWindowFocused:\s*\(\)\s*=>\s*BrowserWindow\.getFocusedWindow/);
	});
});
