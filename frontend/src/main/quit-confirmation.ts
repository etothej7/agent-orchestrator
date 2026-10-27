import { DEFAULT_LOCALE, type AppLocale } from "../shared/ui-locale";
import { mainMessage } from "./main-messages";
import { shouldShowAgentsDialog, type AgentsStateForQuit } from "./quit-agents-dialog";
import { shouldShowSleepDialog, type AwakeStateForQuit } from "./quit-sleep-dialog";

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

/**
 * The dialog's text in the UI locale. The English strings (Ed's wording, kept verbatim in the
 * *_DIALOG_* constants and the two button labels above) live in the catalog and are pinned to those
 * constants by quit-confirmation.test.ts.
 */
export function quitDialogContent(kind: QuitDialogKind, locale: AppLocale = DEFAULT_LOCALE): QuitDialogContent {
	return {
		title: mainMessage(locale, `quit.${kind}.title`),
		body: mainMessage(locale, `quit.${kind}.body`),
		buttons: [mainMessage(locale, "quit.keepRunning"), mainMessage(locale, "quit.quitAnyway")],
		defaultId: 0,
		cancelId: 0,
	};
}

export interface QuitConfirmationDeps {
	/** GET against the daemon; undefined when there is no daemon or it did not answer in time. */
	fetchDaemon(path: string, timeoutMs: number): Promise<unknown | undefined>;
	/** Shows the dialog; resolves to the chosen button index. */
	showDialog(content: QuitDialogContent): Promise<number>;
	/** The UI locale the dialog is shown in; English when absent. */
	locale?(): AppLocale;
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
	return (await deps.showDialog(quitDialogContent(kind, deps.locale?.()))) === 1;
}
