import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";
import { forbiddenDebugSwitch } from "./packaged-hardening";

describe("forbiddenDebugSwitch", () => {
	it.each([
		"--remote-debugging-port=9222",
		"--remote-debugging-port",
		"--remote-debugging-pipe",
		"--remote-debugging-address=0.0.0.0",
		"--remote-allow-origins=*",
		"--inspect",
		"--inspect=9229",
		"--inspect-brk=0.0.0.0:9229",
		"--inspect-port=1",
		"--debug",
		"--debug-brk",
		"--REMOTE-DEBUGGING-PORT=1",
	])("refuses %s", (arg) => {
		expect(forbiddenDebugSwitch(["/Applications/AO.app/Contents/MacOS/AO", "--flag", arg])).toBe(arg);
	});

	it("allows the ordinary launch arguments, and never looks at the executable path", () => {
		expect(forbiddenDebugSwitch(["/path/with--inspect/AO", "--hidden", "--ao-deep-link=ao-app://x", "--user-data-dir=/tmp/x", "/some/folder"])).toBeUndefined();
		expect(forbiddenDebugSwitch(["AO"])).toBeUndefined();
	});
});

describe("Electron fuses for a packaged build", () => {
	const forge = readFileSync(resolve(__dirname, "../../forge.config.ts"), "utf8");
	it.each(["RunAsNode", "EnableNodeOptionsEnvironmentVariable", "EnableNodeCliInspectArguments"])("%s is off", (fuse) => {
		expect(forge).toMatch(new RegExp(`\\[FuseV1Options\\.${fuse}\\]:\\s*false`));
	});
});
