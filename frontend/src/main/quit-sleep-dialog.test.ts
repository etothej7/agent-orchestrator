import { describe, expect, it } from "vitest";
import { shouldShowSleepDialog, SLEEP_DIALOG_BODY, SLEEP_DIALOG_ENABLED, SLEEP_DIALOG_TITLE } from "./quit-sleep-dialog";

describe("sleep dialog", () => {
	it("uses the agreed wording, verbatim", () => {
		expect(SLEEP_DIALOG_TITLE).toBe("Quitting will pause your agents");
		expect(SLEEP_DIALOG_BODY).toBe(
			"Agent Orchestrator is keeping your Mac awake with the lid closed. If you quit, your agents' work will pause until you reopen Agent Orchestrator. Your Mac will stay awake until you reopen it, open the lid, or 12 hours pass. We recommend keeping Agent Orchestrator running so your agents keep working.",
		);
	});

	it("no longer says a quit sleeps or locks the Mac: a quit hands the hold to `ao wake-hold`", () => {
		expect(`${SLEEP_DIALOG_TITLE} ${SLEEP_DIALOG_BODY}`).not.toMatch(/sleep and lock|go to sleep/i);
		// The hold lasts at most 12 hours (backend/internal/cli/wake_hold.go, wakeHoldMaxDuration).
		expect(SLEEP_DIALOG_BODY).toContain("12 hours");
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
