import { describe, expect, it } from "vitest";
import { AGENTS_DIALOG_BODY, AGENTS_DIALOG_ENABLED, AGENTS_DIALOG_TITLE, shouldShowAgentsDialog } from "./quit-agents-dialog";

describe("agents-pause dialog", () => {
	it("uses Ed's wording, verbatim", () => {
		expect(AGENTS_DIALOG_TITLE).toBe("Your agents will pause");
		expect(AGENTS_DIALOG_BODY).toBe(
			"Your agents are still working. If you quit, they will pause until you reopen Agent Orchestrator. We recommend keeping Agent Orchestrator running so your agents keep working.",
		);
	});

	it("is on by default and has a switch", () => {
		expect(AGENTS_DIALOG_ENABLED).toBe(true);
		expect(shouldShowAgentsDialog({ sessions: [{ kind: "worker", activity: { state: "active" } }] }, false)).toBe(false);
	});

	it("shows for a running worker", () => {
		expect(shouldShowAgentsDialog({ sessions: [{ kind: "worker", isTerminated: false, activity: { state: "active" } }] })).toBe(true);
	});

	it("shows for an open autonomous story whose agents are doing the work", () => {
		expect(shouldShowAgentsDialog({ stories: [{ autonomous: true, status: "in_progress" }] })).toBe(true);
		expect(shouldShowAgentsDialog({ stories: [{ autonomous: true, status: "in_review" }] })).toBe(true);
	});

	it("stays quiet when nothing is working", () => {
		expect(shouldShowAgentsDialog(undefined)).toBe(false);
		expect(shouldShowAgentsDialog({})).toBe(false);
		expect(shouldShowAgentsDialog({
			stories: [
				{ autonomous: true, status: "backlog" },
				{ autonomous: true, status: "done" },
				{ autonomous: true, status: "ready_for_approval" },
				{ autonomous: false, status: "in_progress" },
			],
			sessions: [
				{ kind: "worker", activity: { state: "idle" } },
				{ kind: "worker", isTerminated: true, activity: { state: "active" } },
				{ kind: "orchestrator", activity: { state: "active" } },
			],
		})).toBe(false);
	});
});
