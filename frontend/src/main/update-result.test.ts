// @vitest-environment node
import { mkdtemp, readFile, rm, stat, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { UpdateResultError, readUpdateResult, updateResultFile, validateUpdateResult, writeUpdateResult } from "./update-result";

const good = (over: Record<string, unknown> = {}) => ({
	schemaVersion: 1,
	stamp: "20261027T230000Z",
	outcome: "ok",
	fromVersion: "0.14.5-ed.7",
	toVersion: "0.14.5-ed.7.2",
	text: "AO updated to 0.14.5-ed.7.2. Your agents kept running.",
	checks: [{ name: "daemon", result: "ok" }, { name: "sessions", result: "ok" }],
	at: 1_800_000_000_000,
	...over,
});

describe("validateUpdateResult", () => {
	it("accepts a well-formed result for each outcome", () => {
		for (const outcome of ["ok", "rolled_back", "stopped", "deferred"]) {
			expect(validateUpdateResult(good({ outcome }))).toMatchObject({ outcome });
		}
	});

	it("rejects anything that is not the agreed shape", () => {
		for (const bad of [
			null, "x", [], 7,
			good({ schemaVersion: 2 }), good({ stamp: "bad stamp" }), good({ outcome: "great" }),
			good({ fromVersion: "latest" }), good({ toVersion: "" }), good({ text: "" }), good({ text: 5 }),
			good({ checks: "none" }), good({ checks: [{ name: "x", result: "maybe" }] }), good({ checks: [{ name: "", result: "ok" }] }),
			good({ at: "now" }), good({ checks: Array.from({ length: 41 }, (_, i) => ({ name: `c${i}`, result: "ok" })) }),
		]) {
			expect(() => validateUpdateResult(bad), JSON.stringify(bad).slice(0, 80)).toThrow(UpdateResultError);
		}
	});

	it("keeps the message short and drops control characters", () => {
		expect(() => validateUpdateResult(good({ text: "x".repeat(601) }))).toThrow(UpdateResultError);
		expect(validateUpdateResult(good({ text: "line one\u0000\u001b[31m and two" })).text).toBe("line one[31m and two");
	});

	it("refuses text that would leak an address, a host name or a credential", () => {
		for (const text of [
			"reachable at https://example.com/path",
			"see http://127.0.0.1:8080",
			"phone uses 100.101.102.103 now",
			"the tailnet name is mac.tail1234.ts.net",
			"tunnel abc.trycloudflare.com is up",
			"token sk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAA leaked",
			"key ghp_abcdefghijklmnopqrstuvwxyz0123456789 leaked",
			"secret eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.abcdefghijk",
		]) {
			expect(() => validateUpdateResult(good({ text })), text).toThrow(/sensitive/);
		}
	});

	it("refuses sensitive text in a check name too", () => {
		expect(() => validateUpdateResult(good({ checks: [{ name: "mobile https://example.com", result: "ok" }] }))).toThrow(/sensitive/);
	});
});

describe("writeUpdateResult / readUpdateResult", () => {
	let stateDir: string;
	beforeEach(async () => {
		stateDir = await mkdtemp(path.join(os.tmpdir(), "ao-update-result-"));
	});
	afterEach(async () => {
		await rm(stateDir, { recursive: true, force: true });
	});

	it("writes atomically with private permissions under update/runs/<stamp>/result.json", async () => {
		const file = await writeUpdateResult(stateDir, good());
		expect(file).toBe(updateResultFile(stateDir, "20261027T230000Z"));
		expect(file).toBe(path.join(stateDir, "update", "runs", "20261027T230000Z", "result.json"));
		const info = await stat(file);
		expect(info.mode & 0o777).toBe(0o600);
		expect((await stat(path.dirname(file))).mode & 0o777).toBe(0o700);
		expect(JSON.parse(await readFile(file, "utf8"))).toEqual(good());
	});

	it("is write-once: the same result again is fine, a different one is refused", async () => {
		await writeUpdateResult(stateDir, good());
		await expect(writeUpdateResult(stateDir, good())).resolves.toBeTypeOf("string");
		await expect(writeUpdateResult(stateDir, good({ outcome: "rolled_back" }))).rejects.toThrow(/already/);
		expect((await readUpdateResult(stateDir, "20261027T230000Z"))?.outcome).toBe("ok");
	});

	it("writes nothing for an invalid or sensitive result", async () => {
		await expect(writeUpdateResult(stateDir, good({ text: "at https://example.com" }))).rejects.toThrow(UpdateResultError);
		await expect(readUpdateResult(stateDir, "20261027T230000Z")).resolves.toBeUndefined();
	});

	it("reads back only a valid result, and returns undefined for a missing, corrupt or tampered one", async () => {
		expect(await readUpdateResult(stateDir, "20261027T230000Z")).toBeUndefined();
		await writeUpdateResult(stateDir, good());
		await writeFile(updateResultFile(stateDir, "20261027T230000Z"), "{ not json");
		expect(await readUpdateResult(stateDir, "20261027T230000Z")).toBeUndefined();
		await writeFile(updateResultFile(stateDir, "20261027T230000Z"), JSON.stringify(good({ text: "see https://example.com" })));
		expect(await readUpdateResult(stateDir, "20261027T230000Z")).toBeUndefined();
	});

	it("never lets a stamp escape the runs directory", async () => {
		expect(() => updateResultFile(stateDir, "../../etc")).toThrow(UpdateResultError);
		expect(() => updateResultFile(stateDir, "a/b/c/d/e/f")).toThrow(UpdateResultError);
		await expect(readUpdateResult(stateDir, "../x")).resolves.toBeUndefined();
	});
});
