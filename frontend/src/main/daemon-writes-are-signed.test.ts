import { readdirSync, readFileSync } from "node:fs";
import { basename, resolve } from "node:path";
import { describe, expect, it } from "vitest";

/**
 * The daemon refuses a state-changing request that does not carry the person's capability, and main acts for the person
 * too, so every write main makes to the daemon must go through daemonSignedFetch. This test reads the main-process
 * sources and fails when one grows a raw `fetch(` that could be such a write, so a new daemon call is either signed or
 * consciously listed here as going somewhere that is not the daemon.
 */

const MAIN_DIR = __dirname;
const MAIN_TS = resolve(MAIN_DIR, "../main.ts");

/** Files whose raw mutating fetches go to someone else (the cloud, a remote host, a provider, the update feed, the probe). */
const NOT_THE_LOCAL_DAEMON: Record<string, string> = {
	"remotes-main.ts": "remote hosts the person paired, with their own credentials",
	"cloud-auth.ts": "AO Cloud control plane",
	"cloud-auth-local.ts": "a loopback CLOUD control plane, not the AO daemon",
	"provider-auth-flow.ts": "model providers' OAuth endpoints",
	"human-capability-probe.ts": "the key-possession probe, which is the proof signing depends on",
	"daemon-signed-fetch.ts": "the signed fetch itself",
	"auto-updater.ts": "the update feed",
	"mac-differential-v2-transfer.ts": "the update feed",
	"feature-builds.ts": "build download hosts",
	"cloud-agent-proxy.ts": "AO Cloud",
	"cloud-cp-proxy.ts": "AO Cloud",
	"desktop-version-floor.ts": "the version-floor feed",
};

const MUTATING = /method:\s*["'](?:POST|PUT|PATCH|DELETE)["']/i;

function sources(): { file: string; text: string }[] {
	const files = readdirSync(MAIN_DIR)
		.filter((name) => name.endsWith(".ts") && !name.endsWith(".test.ts") && !name.endsWith(".d.ts"))
		.map((name) => resolve(MAIN_DIR, name));
	return [MAIN_TS, ...files].map((file) => ({ file, text: readFileSync(file, "utf8") }));
}

/** Text of each raw `fetch(...)` call (balanced parentheses), not `daemonSignedFetch(` or a method named fetch. */
function rawFetchCalls(text: string): string[] {
	const calls: string[] = [];
	const start = /(?<![\w.$])(?:globalThis\.)?fetch\(/g;
	for (let match = start.exec(text); match; match = start.exec(text)) {
		let depth = 0;
		let end = match.index + match[0].length - 1;
		for (; end < text.length; end += 1) {
			if (text[end] === "(") depth += 1;
			else if (text[end] === ")" && --depth === 0) break;
		}
		calls.push(text.slice(match.index, end + 1));
	}
	return calls;
}

describe("every write main makes to the daemon is signed", () => {
	it("has no raw fetch with a state-changing method outside the files that talk to other services", () => {
		const offenders: string[] = [];
		for (const { file, text } of sources()) {
			if (basename(file) in NOT_THE_LOCAL_DAEMON) continue;
			for (const call of rawFetchCalls(text)) {
				if (MUTATING.test(call)) offenders.push(`${basename(file)}: ${call.replace(/\s+/g, " ").slice(0, 120)}`);
			}
		}
		expect(offenders, "route main's daemon writes through daemonSignedFetch (main/daemon-signed-fetch.ts)").toEqual([]);
	});

	it("passes no caller-supplied init to a raw fetch (it could be a write)", () => {
		const offenders: string[] = [];
		for (const { file, text } of sources()) {
			if (basename(file) in NOT_THE_LOCAL_DAEMON) continue;
			for (const call of rawFetchCalls(text)) {
				if (/fetch\(\s*[^,()]+,\s*(?:init|options|opts|requestInit)\s*\)/.test(call)) offenders.push(`${basename(file)}: ${call.replace(/\s+/g, " ").slice(0, 120)}`);
			}
		}
		expect(offenders).toEqual([]);
	});

	it("sends the known writes through the signed fetch", () => {
		const main = readFileSync(MAIN_TS, "utf8");
		for (const route of ["/side-chats/launch/retire", "${sideBase}/claim", "${sideBase}/state`, {", "/shutdown"]) {
			const at = main.indexOf(route);
			expect(at, route).toBeGreaterThan(-1);
			expect(main.slice(Math.max(0, at - 160), at), `${route} is not sent with daemonSignedFetch`).toContain("daemonSignedFetch(");
		}
		// The two bridges that were handed a raw fetch are handed the signed one.
		expect(main).toMatch(/createAwakeDownloadBridge\(\{[\s\S]{0,300}?request: \(url, init\) => daemonSignedFetch\(url, init\)/);
		expect(main).toMatch(/new TelemetryIdentityController\([\s\S]{0,400}?daemonSignedFetch\(String\(url\), init\)/);
	});

	it("is not vacuous: it recognises a raw write when one is planted", () => {
		const planted = 'await fetch(`http://127.0.0.1:${port}/api/v1/awake/stop`, { method: "POST" });';
		const calls = rawFetchCalls(planted);
		expect(calls).toHaveLength(1);
		expect(MUTATING.test(calls[0] ?? "")).toBe(true);
		expect(rawFetchCalls("await daemonSignedFetch(url, { method: 'POST' }); x.fetch(a); globalThis.fetch(b)").map((c) => c.startsWith("globalThis.fetch("))).toEqual([true]);
	});
});
