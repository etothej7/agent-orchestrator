import { shouldShowAgentsDialog, AGENTS_DIALOG_BODY, AGENTS_DIALOG_TITLE, type AgentsStateForQuit } from "./quit-agents-dialog";
import { shouldShowSleepDialog, SLEEP_DIALOG_BODY, SLEEP_DIALOG_TITLE, type AwakeStateForQuit } from "./quit-sleep-dialog";

export const KEEP_RUNNING_LABEL = "Keep Running";
export const QUIT_ANYWAY_LABEL = "Quit Anyway";
/** The daemon is only asked briefly: a slow or absent daemon must never hold a quit hostage. */
export const QUIT_CHECK_TIMEOUT_MS = 1500;

export type QuitDialogKind = "sleep" | "agents";

export interface QuitDialogContent {
	title: string;
	body: string;
	/** Keep Running is first, the default (Return) and the cancel (Esc) button. */
	buttons: [string, string];
	defaultId: 0;
	cancelId: 0;
}

const buttons: [string, string] = [KEEP_RUNNING_LABEL, QUIT_ANYWAY_LABEL];

export function quitDialogContent(kind: QuitDialogKind): QuitDialogContent {
	return kind === "sleep"
		? { title: SLEEP_DIALOG_TITLE, body: SLEEP_DIALOG_BODY, buttons, defaultId: 0, cancelId: 0 }
		: { title: AGENTS_DIALOG_TITLE, body: AGENTS_DIALOG_BODY, buttons, defaultId: 0, cancelId: 0 };
}

export interface QuitConfirmationDeps {
	/** GET against the daemon; undefined when there is no daemon or it did not answer in time. */
	fetchDaemon(path: string, timeoutMs: number): Promise<unknown | undefined>;
	/** Shows the dialog; resolves to the chosen button index. */
	showDialog(content: QuitDialogContent): Promise<number>;
}

/**
 * Which dialog (if any) a full quit warrants. The sleep dialog wins: if Stay Awake holds a closed-lid
 * Mac, only that one is shown, not both. A daemon that cannot answer means no dialog.
 */
export async function chooseQuitDialog(deps: Pick<QuitConfirmationDeps, "fetchDaemon">): Promise<QuitDialogKind | undefined> {
	const awake = (await deps.fetchDaemon("/api/v1/awake", QUIT_CHECK_TIMEOUT_MS).catch(() => undefined)) as AwakeStateForQuit | undefined;
	if (shouldShowSleepDialog(awake)) return "sleep";
	const [workspace, sessions] = await Promise.all([
		deps.fetchDaemon("/api/v1/workspace", QUIT_CHECK_TIMEOUT_MS).catch(() => undefined),
		deps.fetchDaemon("/api/v1/sessions", QUIT_CHECK_TIMEOUT_MS).catch(() => undefined),
	]);
	const state: AgentsStateForQuit = {
		stories: (workspace as { stories?: AgentsStateForQuit["stories"] } | undefined)?.stories,
		sessions: (sessions as { sessions?: AgentsStateForQuit["sessions"] } | undefined)?.sessions,
	};
	return shouldShowAgentsDialog(state) ? "agents" : undefined;
}

/** True when the quit should go ahead: nothing to warn about, or Quit Anyway was chosen. */
export async function confirmFullQuit(deps: QuitConfirmationDeps): Promise<boolean> {
	const kind = await chooseQuitDialog(deps);
	if (!kind) return true;
	return (await deps.showDialog(quitDialogContent(kind))) === 1;
}
