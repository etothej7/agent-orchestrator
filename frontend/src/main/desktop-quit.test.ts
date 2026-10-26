import { describe, expect, it, vi } from "vitest";
import { createDesktopQuitController, QUIT_FLOW_STALE_MS, shouldQuitWhenAllWindowsClosed } from "./desktop-quit";

/**
 * A stand-in for Electron's app. `app.quit()` raises `before-quit` (our two handlers); unless one of them
 * held it, `will-quit` fires and the process exits. `app.quit()` called directly is what an AppleEvent,
 * a logout or a Dock "Quit" does: it did not come through one of the controller's entry points.
 */
function harness(options: {
	drafts?: number | (() => number);
	confirmDraft?: boolean[];
	warn?: boolean[] | (() => Promise<boolean>);
	updateRestart?: boolean;
	clock?: { value: number };
} = {}) {
	const draftAnswers = [...(options.confirmDraft ?? [])];
	const warnAnswers = Array.isArray(options.warn) ? [...options.warn] : undefined;
	const log: string[] = [];
	let exits = 0;
	let vetoNext = false;
	const controller = createDesktopQuitController({
		draftRiskCount: () => (typeof options.drafts === "function" ? options.drafts() : options.drafts ?? 0),
		confirmDraftLeave: () => {
			log.push("draft?");
			return draftAnswers.shift() ?? false;
		},
		quit: () => app.quit(),
		relaunch: () => log.push("relaunch"),
		isUpdateRestart: () => options.updateRestart ?? false,
		now: () => options.clock?.value ?? 0,
		confirmFullQuit: async () => {
			log.push("warn?");
			if (typeof options.warn === "function") return options.warn();
			return warnAnswers?.shift() ?? true;
		},
	});
	const app = {
		quit() {
			log.push("quit");
			const event = { prevented: false, preventDefault() { this.prevented = true; } };
			if (controller.handleBeforeQuit(event) || controller.holdForWarnings(event) || event.prevented) return;
			if (vetoNext) {
				// Something else (a window that would not unload) stopped the quit after our handlers passed it.
				vetoNext = false;
				log.push("vetoed");
				return;
			}
			controller.handleWillQuit();
			exits += 1;
		},
	};
	return { controller, app, log, exits: () => exits, vetoNext: () => { vetoNext = true; } };
}

const settle = () => new Promise<void>((resolve) => setTimeout(resolve, 0));

describe("closing the window", () => {
	it("keeps AO running on macOS and quits elsewhere", () => {
		expect(shouldQuitWhenAllWindowsClosed("darwin")).toBe(false);
		expect(shouldQuitWhenAllWindowsClosed("win32")).toBe(true);
		expect(shouldQuitWhenAllWindowsClosed("linux")).toBe(true);
	});
});

describe("which quits are warned (agents will pause / Mac will sleep)", () => {
	it("the app menu's Quit and Cmd-Q (requestUserQuit) are warned, then exit on Quit Anyway", async () => {
		const t = harness({ warn: [true] });
		t.controller.requestUserQuit();
		expect(t.exits()).toBe(0);
		await settle();
		expect(t.log).toEqual(["quit", "warn?", "quit"]);
		expect(t.exits()).toBe(1);
	});

	it("the tray's Quit AO Completely uses the same user quit and is warned", async () => {
		const t = harness({ warn: [false] });
		t.controller.requestUserQuit();
		await settle();
		expect(t.log).toEqual(["quit", "warn?"]);
		expect(t.exits()).toBe(0);
	});

	it("an AppleEvent / installer quit is never blocked by the warnings", async () => {
		const t = harness({ warn: [false] });
		t.app.quit();
		await settle();
		expect(t.log).toEqual(["quit"]);
		expect(t.exits()).toBe(1);
	});

	it("a logout or restart that arrives as a plain quit is never blocked by the warnings", async () => {
		const t = harness({ warn: [false] });
		t.app.quit();
		await settle();
		expect(t.exits()).toBe(1);
		expect(t.log).not.toContain("warn?");
	});

	it("an OS shutdown is never warned", async () => {
		const t = harness({ warn: [false] });
		t.controller.quitForShutdown();
		await settle();
		expect(t.log).toEqual(["quit"]);
		expect(t.exits()).toBe(1);
	});

	it("an update relaunch is never warned, and relaunches at will-quit", async () => {
		const t = harness({ warn: [false] });
		t.controller.relaunchAndQuit();
		await settle();
		expect(t.log).toEqual(["quit", "relaunch"]);
		expect(t.exits()).toBe(1);
	});

	it("an updater-driven restart is never warned, even when the person pressed Quit", async () => {
		const t = harness({ warn: [false], updateRestart: true });
		t.controller.requestUserQuit();
		await settle();
		expect(t.log).toEqual(["quit"]);
		expect(t.exits()).toBe(1);
	});

	it("a failed check never traps the person in a running app", async () => {
		const t = harness({ warn: () => Promise.reject(new Error("daemon gone")) });
		t.controller.requestUserQuit();
		await settle();
		expect(t.exits()).toBe(1);
	});

	it("does not stack a second check while one is open", async () => {
		let release!: (value: boolean) => void;
		const t = harness({ warn: () => new Promise<boolean>((resolve) => { release = resolve; }) });
		t.controller.requestUserQuit();
		t.app.quit();
		expect(t.log.filter((entry) => entry === "warn?")).toHaveLength(1);
		release(false);
		await settle();
	});
});

describe("a cancelled quit leaves nothing behind", () => {
	it("Keep Running: the next Cmd-Q warns again", async () => {
		const t = harness({ warn: [false, true] });
		t.controller.requestUserQuit();
		await settle();
		expect(t.exits()).toBe(0);
		t.controller.requestUserQuit();
		await settle();
		expect(t.log).toEqual(["quit", "warn?", "quit", "warn?", "quit"]);
		expect(t.exits()).toBe(1);
	});

	it("Keep Running does not make a later AppleEvent quit warn or hang", async () => {
		const t = harness({ warn: [false] });
		t.controller.requestUserQuit();
		await settle();
		t.app.quit();
		await settle();
		expect(t.log).toEqual(["quit", "warn?", "quit"]);
		expect(t.exits()).toBe(1);
	});

	it("a quit that passed the warnings but was then vetoed: the next Cmd-Q warns again", async () => {
		const t = harness({ warn: [true, true] });
		t.vetoNext();
		t.controller.requestUserQuit();
		await settle();
		expect(t.exits()).toBe(0);
		expect(t.log).toEqual(["quit", "warn?", "quit", "vetoed"]);
		t.controller.requestUserQuit();
		await settle();
		expect(t.log.filter((entry) => entry === "warn?")).toHaveLength(2);
		expect(t.exits()).toBe(1);
	});

	it("a relaunch requested for a quit that was vetoed never fires at a later exit", async () => {
		const clock = { value: 0 };
		const t = harness({ clock });
		t.vetoNext();
		t.controller.relaunchAndQuit();
		await settle();
		expect(t.exits()).toBe(0);
		// The vetoed quit went quiet; a later, unrelated quit starts a fresh flow.
		clock.value += QUIT_FLOW_STALE_MS + 1;
		t.app.quit();
		await settle();
		expect(t.exits()).toBe(1);
		expect(t.log).not.toContain("relaunch");
	});

	it("a relaunch requested for a quit that a late draft held back never fires at a later exit", async () => {
		let drafts = 0;
		const t = harness({ drafts: () => drafts, confirmDraft: [false] });
		const original = t.app.quit.bind(t.app);
		t.app.quit = () => { drafts = 1; original(); };
		t.controller.relaunchAndQuit();
		expect(t.exits()).toBe(0);
		drafts = 0;
		t.app.quit = original;
		t.app.quit();
		expect(t.exits()).toBe(1);
		expect(t.log).not.toContain("relaunch");
	});

	it("a plain quit never relaunches", () => {
		const t = harness();
		t.app.quit();
		expect(t.log).toEqual(["quit"]);
	});
});

describe("unsaved drafts", () => {
	it("hold every kind of quit until the person agrees, then exit without asking twice", async () => {
		for (const start of [
			(t: ReturnType<typeof harness>) => t.controller.requestUserQuit(),
			(t: ReturnType<typeof harness>) => t.app.quit(),
			(t: ReturnType<typeof harness>) => t.controller.quitForShutdown(),
		]) {
			const t = harness({ drafts: 1, confirmDraft: [true], warn: [true] });
			start(t);
			await settle();
			expect(t.log.filter((entry) => entry === "draft?")).toHaveLength(1);
			expect(t.exits()).toBe(1);
		}
	});

	it("asks about drafts before the warnings", async () => {
		const t = harness({ drafts: 1, confirmDraft: [true], warn: [true] });
		t.controller.requestUserQuit();
		await settle();
		expect(t.log.slice(0, 3)).toEqual(["quit", "draft?", "quit"]);
		expect(t.log).toContain("warn?");
		expect(t.log.indexOf("draft?")).toBeLessThan(t.log.indexOf("warn?"));
	});

	it("declining keeps AO running and is asked again on the next quit", () => {
		const t = harness({ drafts: 1, confirmDraft: [false, true] });
		t.app.quit();
		expect(t.exits()).toBe(0);
		t.app.quit();
		expect(t.log).toEqual(["quit", "draft?", "quit", "draft?", "quit"]);
		expect(t.exits()).toBe(1);
	});

	it("an update relaunch asks first, and declining leaves no relaunch armed", () => {
		const declined = harness({ drafts: 2, confirmDraft: [false] });
		declined.controller.relaunchAndQuit();
		expect(declined.log).toEqual(["draft?"]);
		declined.app.quit();
		expect(declined.log).not.toContain("relaunch");
		const accepted = harness({ drafts: 2, confirmDraft: [true] });
		accepted.controller.relaunchAndQuit();
		expect(accepted.log).toEqual(["draft?", "quit", "relaunch"]);
		expect(accepted.exits()).toBe(1);
	});

	it("tells the window-close handler a confirmed quit need not ask again, until reset", () => {
		const t = harness({ drafts: 1, confirmDraft: [true] });
		expect(t.controller.draftLeaveConfirmed()).toBe(false);
		t.app.quit();
		expect(t.controller.draftLeaveConfirmed()).toBe(true);
		t.controller.resetDraftConfirmation();
		expect(t.controller.draftLeaveConfirmed()).toBe(false);
	});

	it("Cmd-Q is not intercepted when nothing is unsaved", () => {
		const t = harness();
		const event = { preventDefault: vi.fn() };
		expect(t.controller.handleBeforeQuit(event)).toBe(false);
		expect(event.preventDefault).not.toHaveBeenCalled();
	});
});
