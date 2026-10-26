import { describe, expect, it, vi } from "vitest";
import {
	chooseQuitDialog, confirmFullQuit, KEEP_RUNNING_LABEL, QUIT_ANYWAY_LABEL, QUIT_CHECK_TIMEOUT_MS, quitDialogContent, type QuitConfirmationDeps,
} from "./quit-confirmation";

const awake = { active: true, lidActive: true };
const working = { stories: [{ autonomous: true, status: "in_progress" }] };

function deps(answers: Record<string, unknown>, choice = 0) {
	const fetchDaemon = vi.fn(async (path: string) => answers[path]);
	const showDialog = vi.fn(async () => choice);
	return { fetchDaemon, showDialog } satisfies QuitConfirmationDeps;
}

describe("quit dialogs", () => {
	it("both offer Keep Running (default, Return and Esc) and Quit Anyway", () => {
		for (const kind of ["sleep", "agents"] as const) {
			const content = quitDialogContent(kind);
			expect(content.buttons).toEqual(["Keep Running", "Quit Anyway"]);
			expect(content.defaultId).toBe(0);
			expect(content.cancelId).toBe(0);
		}
		expect([KEEP_RUNNING_LABEL, QUIT_ANYWAY_LABEL]).toEqual(["Keep Running", "Quit Anyway"]);
	});

	it("carries the verbatim titles", () => {
		expect(quitDialogContent("sleep").title).toBe("Your Mac will go to sleep and lock");
		expect(quitDialogContent("agents").title).toBe("Your agents will pause");
	});

	it("shows only the sleep dialog when both conditions apply", async () => {
		const d = deps({ "/api/v1/awake": awake, "/api/v1/workspace": working, "/api/v1/sessions": { sessions: [{ kind: "worker", activity: { state: "active" } }] } });
		expect(await chooseQuitDialog(d)).toBe("sleep");
		expect(d.fetchDaemon).not.toHaveBeenCalledWith("/api/v1/workspace", expect.anything());
	});

	it("shows the agents dialog for open autonomous work or a running worker", async () => {
		expect(await chooseQuitDialog(deps({ "/api/v1/awake": { active: false }, "/api/v1/workspace": working }))).toBe("agents");
		expect(await chooseQuitDialog(deps({ "/api/v1/sessions": { sessions: [{ kind: "worker", activity: { state: "active" } }] } }))).toBe("agents");
	});

	it("shows nothing when nothing is active", async () => {
		expect(await chooseQuitDialog(deps({ "/api/v1/awake": { active: false, lidActive: false }, "/api/v1/workspace": { stories: [] }, "/api/v1/sessions": { sessions: [] } }))).toBeUndefined();
	});

	it("asks the daemon only briefly, and an unreachable or failing daemon means no dialog", async () => {
		const d = deps({});
		await chooseQuitDialog(d);
		expect(d.fetchDaemon).toHaveBeenCalledWith("/api/v1/awake", QUIT_CHECK_TIMEOUT_MS);
		expect(QUIT_CHECK_TIMEOUT_MS).toBeLessThanOrEqual(2000);
		const failing: QuitConfirmationDeps = { fetchDaemon: async () => { throw new Error("refused"); }, showDialog: vi.fn(async () => 0) };
		expect(await chooseQuitDialog(failing)).toBeUndefined();
		expect(await confirmFullQuit(failing)).toBe(true);
		expect(failing.showDialog).not.toHaveBeenCalled();
	});

	it("Keep Running cancels the quit; Quit Anyway lets it through", async () => {
		const answers = { "/api/v1/awake": awake };
		expect(await confirmFullQuit(deps(answers, 0))).toBe(false);
		expect(await confirmFullQuit(deps(answers, 1))).toBe(true);
	});

	it("does not ask at all when nothing is active", async () => {
		const d = deps({ "/api/v1/awake": { active: false } });
		expect(await confirmFullQuit(d)).toBe(true);
		expect(d.showDialog).not.toHaveBeenCalled();
	});
});
