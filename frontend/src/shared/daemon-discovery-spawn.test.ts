import { describe, expect, it } from "vitest";
import { parseRunFile, runFileIsFromThisSpawn } from "./daemon-discovery";

const spawn = { pid: 4242, appRunId: "run-abc", spawnedAtMs: Date.parse("2026-10-27T12:00:00Z"), skewMs: 2000 };
const file = (overrides: Record<string, unknown> = {}) =>
	parseRunFile(JSON.stringify({ pid: 4242, port: 41234, startedAt: "2026-10-27T12:00:01Z", appRunId: "run-abc", ...overrides }))!;

describe("runFileIsFromThisSpawn", () => {
	it("accepts the handshake of the child this run just spawned", () => {
		expect(runFileIsFromThisSpawn(file(), spawn)).toBe(true);
	});

	it("refuses a forged or foreign run file", () => {
		expect(runFileIsFromThisSpawn(file({ pid: 777 }), spawn), "another process's pid").toBe(false);
		expect(runFileIsFromThisSpawn(file({ pid: 0 }), spawn), "no pid").toBe(false);
		expect(runFileIsFromThisSpawn(file({ appRunId: "other-run" }), spawn), "another app run").toBe(false);
		expect(runFileIsFromThisSpawn(file({ appRunId: undefined }), spawn), "no app run id").toBe(false);
		expect(runFileIsFromThisSpawn(file({ startedAt: "2026-10-27T11:00:00Z" }), spawn), "a stale file").toBe(false);
		expect(runFileIsFromThisSpawn(file({ startedAt: "garbage" }), spawn), "no start time").toBe(false);
		expect(runFileIsFromThisSpawn(file(), { ...spawn, pid: undefined }), "a child with no pid").toBe(false);
	});
});
