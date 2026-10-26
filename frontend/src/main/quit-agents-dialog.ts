/**
 * Shown on a full quit while autonomous work is active: quitting pauses it. The wording is Ed's,
 * verbatim. Set AGENTS_DIALOG_ENABLED to false to switch the dialog off.
 */
export const AGENTS_DIALOG_ENABLED = true;

export const AGENTS_DIALOG_TITLE = "Your agents will pause";
export const AGENTS_DIALOG_BODY =
	"Your agents are still working. If you quit, they will pause until you reopen Agent Orchestrator. We recommend keeping Agent Orchestrator running so your agents keep working.";

/** The parts of the daemon's answers this dialog reads (GET /api/v1/workspace and /api/v1/sessions). */
export interface AgentsStateForQuit {
	stories?: ReadonlyArray<{ autonomous?: boolean; status?: string }>;
	sessions?: ReadonlyArray<{ kind?: string; isTerminated?: boolean; activity?: { state?: string } }>;
}

/** Story statuses in which agents are doing the work (not backlog, not waiting on Ed, not done). */
const WORKING_STORY_STATUSES: ReadonlySet<string> = new Set(["in_progress", "in_review"]);

/** An open autonomous story, or a worker that is running right now. */
export function shouldShowAgentsDialog(state: AgentsStateForQuit | undefined, enabled = AGENTS_DIALOG_ENABLED): boolean {
	if (!enabled || !state) return false;
	const openStory = state.stories?.some((story) => story.autonomous === true && WORKING_STORY_STATUSES.has(story.status ?? ""));
	const runningWorker = state.sessions?.some((session) => session.kind === "worker" && session.isTerminated !== true && session.activity?.state === "active");
	return Boolean(openStory || runningWorker);
}
