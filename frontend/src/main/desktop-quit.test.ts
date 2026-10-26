import { describe, expect, it, vi } from "vitest";
import { createDesktopQuitController, shouldQuitWhenAllWindowsClosed } from "./desktop-quit";

/**
 * A stand-in for Electron's app: `quit()` raises `before-quit`; the app exits unless the
 * handler prevented it.
 */
function harness(options: { drafts?: number | (() => number); confirm?: boolean[]; warnings?: { proceed: boolean | (() => Promise<boolean>); updateRestart?: boolean } } = {}) {
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
		isUpdateRestart: () => options.warnings?.updateRestart ?? false,
		confirmFullQuit: async () => {
			log.push("warn");
			const proceed = options.warnings?.proceed ?? true;
			return typeof proceed === "function" ? proceed() : proceed;
		},
	});
	const app = {
		quit() {
			log.push("quit");
			const event = { prevented: false, preventDefault() { this.prevented = true; } };
			if (options.warnings && !controller.handleBeforeQuit(event) && controller.holdForWarnings(event)) return;
			if (!options.warnings ? !controller.handleBeforeQuit(event) && !event.prevented : !event.prevented) {
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

const settle = () => new Promise<void>((resolve) => setTimeout(resolve, 0));

describe("warnings before a full quit", () => {
	it("holds a person-initiated quit for the check, then exits when nothing needs a warning", async () => {
		const { app, log, exits } = harness({ warnings: { proceed: true } });
		app.quit();
		expect(exits()).toBe(0);
		await settle();
		expect(log).toEqual(["quit", "warn", "quit"]);
		expect(exits()).toBe(1);
	});

	it("Keep Running cancels the quit, and the next quit asks again", async () => {
		const answers = [false, true];
		const { app, log, exits } = harness({ warnings: { proceed: async () => answers.shift() ?? true } });
		app.quit();
		await settle();
		expect(exits()).toBe(0);
		expect(log).toEqual(["quit", "warn"]);
		app.quit();
		await settle();
		expect(log).toEqual(["quit", "warn", "quit", "warn", "quit"]);
		expect(exits()).toBe(1);
	});

	it("does not stack a second check while one is open", async () => {
		let release!: (value: boolean) => void;
		const { app, log } = harness({ warnings: { proceed: () => new Promise<boolean>((resolve) => { release = resolve; }) } });
		app.quit();
		app.quit();
		expect(log.filter((entry) => entry === "warn")).toHaveLength(1);
		release(false);
		await settle();
	});

	it("never traps the person: a check that throws lets the quit through", async () => {
		const { app, exits } = harness({ warnings: { proceed: () => Promise.reject(new Error("daemon gone")) } });
		app.quit();
		await settle();
		expect(exits()).toBe(1);
	});

	it("asks about unsaved drafts first, then about agents", async () => {
		const { app, log, exits } = harness({ drafts: 1, confirm: [true], warnings: { proceed: true } });
		app.quit();
		await settle();
		expect(log).toEqual(["quit", "ask", "quit", "warn", "quit"]);
		expect(exits()).toBe(1);
	});

	it("an OS shutdown quits without any warning", async () => {
		const { controller, log, exits } = harness({ warnings: { proceed: false } });
		controller.quitForShutdown();
		await settle();
		expect(log).toEqual(["quit"]);
		expect(exits()).toBe(1);
	});

	it("an update relaunch quits without any warning and still relaunches", async () => {
		const { controller, log, exits } = harness({ warnings: { proceed: false } });
		controller.relaunchAndQuit();
		await settle();
		expect(log).toEqual(["quit", "relaunch"]);
		expect(exits()).toBe(1);
	});

	it("an updater-driven restart quits without any warning", async () => {
		const { app, log, exits } = harness({ warnings: { proceed: false, updateRestart: true } });
		app.quit();
		await settle();
		expect(log).toEqual(["quit"]);
		expect(exits()).toBe(1);
	});

	it("the tray's Quit AO Completely is warned like any other quit", async () => {
		const { controller, log, exits } = harness({ warnings: { proceed: false } });
		controller.quit();
		await settle();
		expect(log).toEqual(["quit", "warn"]);
		expect(exits()).toBe(0);
	});
});
