import { describe, expect, it } from "vitest";
import { isPersonalBuild } from "./personal-build";

describe("personal build lineage", () => {
	it.each(["0.14.5-ed.3", "0.14.5-ed.4", "0.13.0-ed.workspace.1"])("keeps %s local", version => {
		expect(isPersonalBuild(version)).toBe(true);
	});
	it.each(["0.14.5", "0.14.6-nightly.1", "0.14.6-pr123.1", "0.14.5-editor.1"])("preserves release behavior for %s", version => {
		expect(isPersonalBuild(version)).toBe(false);
	});
});
