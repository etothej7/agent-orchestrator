import { describe, expect, it } from "vitest";
import { shouldShowSleepDialog, SLEEP_DIALOG_BODY, SLEEP_DIALOG_ENABLED, SLEEP_DIALOG_TITLE } from "./quit-sleep-dialog";

describe("sleep dialog", () => {
	it("uses Ed's wording, verbatim", () => {
		expect(SLEEP_DIALOG_TITLE).toBe("Your Mac will go to sleep and lock");
		expect(SLEEP_DIALOG_BODY).toBe(
			"Agent Orchestrator is keeping your Mac awake with the lid closed. If you quit, your Mac will go to sleep and lock, and your agents' work will pause until you wake it and reopen Agent Orchestrator. We recommend keeping Agent Orchestrator running so your Mac stays awake and your agents keep working.",
		);
	});

	it("is on by default and has a switch", () => {
		expect(SLEEP_DIALOG_ENABLED).toBe(true);
		expect(shouldShowSleepDialog({ active: true, lidActive: true }, false)).toBe(false);
	});

	it("shows only while Stay Awake is active and armed for a closed lid", () => {
		expect(shouldShowSleepDialog({ active: true, lidActive: true })).toBe(true);
		expect(shouldShowSleepDialog({ active: true, lidActive: false })).toBe(false);
		expect(shouldShowSleepDialog({ active: false, lidActive: true })).toBe(false);
		expect(shouldShowSleepDialog({})).toBe(false);
		expect(shouldShowSleepDialog(undefined)).toBe(false);
	});
});
