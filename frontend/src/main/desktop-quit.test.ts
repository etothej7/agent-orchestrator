import { describe, expect, it, vi } from "vitest";
import { createDesktopQuitController, shouldQuitWhenAllWindowsClosed } from "./desktop-quit";

/**
 * A stand-in for Electron's app: `quit()` raises `before-quit`; the app exits unless the
 * handler prevented it.
 */
function harness(options: { drafts?: number | (() => number); confirm?: boolean[] } = {}) {
	const answers = [...(options.confirm ?? [])];
	const log: string[] = [];
	let exited = 0;
	const controller = createDesktopQuitController({
		draftRiskCount: () => (typeof options.drafts === "function" ? options.drafts() : options.drafts ?? 0),
		confirmDraftLeave: () => {
			log.push("ask");
			return answers.shift() ?? false;
		},
		quit: () => app.quit(),
		relaunch: () => log.push("relaunch"),
	});
	const app = {
		quit() {
			log.push("quit");
			const event = { prevented: false, preventDefault() { this.prevented = true; } };
			if (!controller.handleBeforeQuit(event) && !event.prevented) {
				// Nothing stopped it: will-quit fires, then the process exits.
				controller.handleWillQuit();
				exited += 1;
			}
		},
	};
	return { controller, app, log, exits: () => exited };
}

describe("desktop quit", () => {
	it("closing the last window keeps AO running on macOS and quits elsewhere", () => {
		expect(shouldQuitWhenAllWindowsClosed("darwin")).toBe(false);
		expect(shouldQuitWhenAllWindowsClosed("win32")).toBe(true);
		expect(shouldQuitWhenAllWindowsClosed("linux")).toBe(true);
	});

	it("Cmd-Q, the app-menu Quit and an AppleEvent quit are a full exit: before-quit is not intercepted", () => {
		const { controller } = harness();
		const event = { preventDefault: vi.fn() };
		expect(controller.handleBeforeQuit(event)).toBe(false);
		expect(event.preventDefault).not.toHaveBeenCalled();
	});

	it("exits for real when quit is requested with nothing unsaved", () => {
		const { app, exits } = harness();
		app.quit();
		expect(exits()).toBe(1);
	});

	it("the tray's Quit AO Completely exits", () => {
		const { controller, exits } = harness();
		controller.quit();
		expect(exits()).toBe(1);
	});

	it("an OS shutdown exits", () => {
		const { controller, exits } = harness();
		controller.quit();
		expect(exits()).toBe(1);
	});

	it("an update relaunch quits, and starts the new AO only once the quit cannot be stopped (will-quit)", () => {
		const { controller, log, exits } = harness();
		controller.relaunchAndQuit();
		expect(log).toEqual(["quit", "relaunch"]);
		expect(exits()).toBe(1);
	});

	it("a plain quit never relaunches", () => {
		const { app, log } = harness();
		app.quit();
		expect(log).toEqual(["quit"]);
	});

	describe("unsaved drafts", () => {
		it("hold a quit until the person agrees, then exit without asking twice", () => {
			const { app, log, exits } = harness({ drafts: 1, confirm: [true] });
			app.quit();
			expect(log).toEqual(["quit", "ask", "quit"]);
			expect(exits()).toBe(1);
		});

		it("declining keeps AO running and is asked again on the next quit", () => {
			const { app, log, exits } = harness({ drafts: 1, confirm: [false, true] });
			app.quit();
			expect(exits()).toBe(0);
			expect(log).toEqual(["quit", "ask"]);
			app.quit();
			expect(log).toEqual(["quit", "ask", "quit", "ask", "quit"]);
			expect(exits()).toBe(1);
		});

		it("also guard the tray quit and shutdown paths", () => {
			const declined = harness({ drafts: 2, confirm: [false] });
			declined.controller.quit();
			expect(declined.exits()).toBe(0);
		});

		it("ask before an update relaunch is armed, so declining leaves no relaunch behind", () => {
			const declined = harness({ drafts: 2, confirm: [false] });
			declined.controller.relaunchAndQuit();
			expect(declined.log).toEqual(["ask"]);
			expect(declined.exits()).toBe(0);
			// A later plain quit exits without starting a stray new AO.
			declined.app.quit();
			expect(declined.log).not.toContain("relaunch");
		});

		it("relaunches once the draft prompt is accepted, without asking again", () => {
			const accepted = harness({ drafts: 2, confirm: [true] });
			accepted.controller.relaunchAndQuit();
			expect(accepted.log).toEqual(["ask", "quit", "relaunch"]);
			expect(accepted.exits()).toBe(1);
		});

		it("a draft that appears after the relaunch was requested holds the quit and leaves no relaunch behind", () => {
			let drafts = 0;
			const raced = harness({ drafts: () => drafts, confirm: [false] });
			// relaunchAndQuit sees no drafts, then one appears before before-quit runs.
			const quit = raced.controller.relaunchAndQuit.bind(raced.controller);
			drafts = 0;
			const original = raced.app.quit.bind(raced.app);
			raced.app.quit = () => { drafts = 1; original(); };
			quit();
			expect(raced.exits()).toBe(0);
			expect(raced.log).toEqual(["quit", "ask"]);
			// The drafts are gone; a later plain quit must not start a new AO.
			drafts = 0;
			raced.app.quit = original;
			raced.app.quit();
			expect(raced.exits()).toBe(1);
			expect(raced.log).not.toContain("relaunch");
		});

		it("tells the window-close handler a confirmed quit need not ask again, until reset", () => {
			const { app, controller } = harness({ drafts: 1, confirm: [true] });
			expect(controller.draftLeaveConfirmed()).toBe(false);
			app.quit();
			expect(controller.draftLeaveConfirmed()).toBe(true);
			controller.resetDraftConfirmation();
			expect(controller.draftLeaveConfirmed()).toBe(false);
		});
	});
});
