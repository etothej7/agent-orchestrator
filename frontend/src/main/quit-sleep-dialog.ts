/**
 * Shown on a full quit while AO's Stay Awake is holding a closed-lid Mac. A quit no longer lets the Mac
 * sleep and lock: it starts a detached `ao wake-hold` that keeps the Mac awake until AO is reopened, the
 * lid is opened, or 12 hours pass (quit-holdover.ts). What a quit does cost is the agents' work.
 * The wording was reworded with Ed on 2026-10-27. Set SLEEP_DIALOG_ENABLED to false to switch the dialog off.
 */
export const SLEEP_DIALOG_ENABLED = true;

export const SLEEP_DIALOG_TITLE = "Quitting will pause your agents";
export const SLEEP_DIALOG_BODY =
	"Agent Orchestrator is keeping your Mac awake with the lid closed. If you quit, your agents' work will pause until you reopen Agent Orchestrator. Your Mac will stay awake until you reopen it, open the lid, or 12 hours pass. We recommend keeping Agent Orchestrator running so your agents keep working.";

/** The part of the daemon's awake state this dialog reads (GET /api/v1/awake). */
export interface AwakeStateForQuit {
	active?: boolean;
	lidActive?: boolean;
}

/** Stay Awake is on and armed for a closed lid. */
export function shouldShowSleepDialog(state: AwakeStateForQuit | undefined, enabled = SLEEP_DIALOG_ENABLED): boolean {
	return enabled && state?.active === true && state.lidActive === true;
}
