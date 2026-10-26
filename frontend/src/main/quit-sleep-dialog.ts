/**
 * Shown on a full quit while AO's Stay Awake is holding a closed-lid Mac: quitting lets it sleep and lock.
 * The wording is Ed's, verbatim. Set SLEEP_DIALOG_ENABLED to false to switch the dialog off.
 */
export const SLEEP_DIALOG_ENABLED = true;

export const SLEEP_DIALOG_TITLE = "Your Mac will go to sleep and lock";
export const SLEEP_DIALOG_BODY =
	"Agent Orchestrator is keeping your Mac awake with the lid closed. If you quit, your Mac will go to sleep and lock, and your agents' work will pause until you wake it and reopen Agent Orchestrator. We recommend keeping Agent Orchestrator running so your Mac stays awake and your agents keep working.";

/** The part of the daemon's awake state this dialog reads (GET /api/v1/awake). */
export interface AwakeStateForQuit {
	active?: boolean;
	lidActive?: boolean;
}

/** Stay Awake is on and armed for a closed lid. */
export function shouldShowSleepDialog(state: AwakeStateForQuit | undefined, enabled = SLEEP_DIALOG_ENABLED): boolean {
	return enabled && state?.active === true && state.lidActive === true;
}
