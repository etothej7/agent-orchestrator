import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { CoordinatorUnread } from "../../hooks/useReadCoordinatorTells";
import { useCoordinatorJumpStore } from "../../stores/coordinator-jump-store";
import { typeInLexicalEditor } from "../../test/lexical";
import { chatFixtureEmpty } from "../../lib/chat-fixture";
import type { ConversationAudienceTag, ConversationItem, ConversationSnapshot, ConversationTurn } from "../../types/conversation";
import { TooltipProvider } from "../ui/tooltip";
import { ChatWorkspace } from "./ChatWorkspace";

/**
 * `ao tell` / `ao ask` write a daemon message with no turn (origin daemon, role assistant,
 * audience human) while the coordinator may be in the middle of a turn. It must be on
 * screen in both views, whether it arrives with the page or live.
 */
const at = (minute: number) => `2026-10-26T10:${String(minute % 59).padStart(2, "0")}:00Z`;
const turn = (id: string, minute: number, state: ConversationTurn["state"], audience?: ConversationAudienceTag): ConversationTurn => ({
	id, state, requestedAt: at(minute), startedAt: at(minute), completedAt: state === "running" ? undefined : at(minute + 1), hasHumanPrompt: audience !== "internal", audience,
});
const message = (id: string, turnId: string | undefined, sequence: number, role: "user" | "assistant", origin: "human" | "automation" | "daemon" | "provider", text: string, audience?: ConversationAudienceTag, purpose?: string): ConversationItem => ({
	kind: "message", id, turnId, sequence, revision: 1, role, origin, text, streaming: false, audience, purpose, createdAt: at(sequence),
});
const tell = (sequence: number, audience?: ConversationAudienceTag) => message("tell", undefined, sequence, "assistant", "daemon", "Verified: the install succeeded.", audience, "verified_result");
const ask = (sequence: number, audience?: ConversationAudienceTag) => message("decision-q1", undefined, sequence, "assistant", "daemon", "Which migration order should we use?\n\nWhy your input is needed: both touch billing.", audience, "blocking_decision");

function snapshot(items: ConversationItem[], turns: ConversationTurn[], running = false): ConversationSnapshot {
	return {
		...chatFixtureEmpty, controller: { state: running ? "busy" : "ready" }, capabilities: [],
		latestSequence: Math.max(...items.map((item) => item.sequence)) + 1, oldestSequence: 1, hasMoreBefore: false, turns, items,
	};
}
const view = (snap: ConversationSnapshot) => <TooltipProvider><ChatWorkspace snapshot={snap} /></TooltipProvider>;
const manyEarlierTurns = (audience?: ConversationAudienceTag) => {
	const turns: ConversationTurn[] = [];
	const items: ConversationItem[] = [];
	for (let i = 0; i < 30; i += 1) {
		turns.push(turn(`old${i}`, i, "completed", audience && "human"));
		items.push(message(`ou${i}`, `old${i}`, i * 2 + 1, "user", "human", `Earlier question ${i}`, audience && "human"));
		items.push(message(`oa${i}`, `old${i}`, i * 2 + 2, "assistant", "provider", `Earlier answer ${i}`, audience && "human"));
	}
	return { turns, items };
};

describe.each([
	["the default view", undefined],
	["the internal view", "human" as const],
])("a tell or ask inside an Ed-started turn, %s", (_name, tag) => {
	it.each([["completed", false], ["running", true]] as const)("is on screen while the turn is %s", (state, running) => {
		const items = [
			message("u", "ed", 740, "user", "human", "Is it installed?", tag),
			message("a1", "ed", 741, "assistant", "provider", "Checking.", tag),
			tell(742, tag),
			message("a2", "ed", 743, "assistant", "provider", "It is installed.", tag),
		];
		render(view(snapshot(items, [turn("ed", 50, state, tag)], running)));
		expect(screen.getByText("Verified: the install succeeded.")).toBeInTheDocument();
		// A deliberate message to Ed is never marked internal.
		for (const region of document.querySelectorAll('[data-audience="internal"]')) {
			expect(within(region as HTMLElement).queryByText("Verified: the install succeeded.")).not.toBeInTheDocument();
		}
	});

	it("appears without a reload when it arrives while the turn is running", () => {
		const base = (withTell: boolean) => snapshot([
			message("u", "ed", 740, "user", "human", "Is it installed?", tag),
			message("a1", "ed", 741, "assistant", "provider", "Checking.", tag),
			...(withTell ? [tell(742, tag)] : []),
			message("a2", "ed", 743, "assistant", "provider", "Still checking.", tag),
		], [turn("ed", 50, "running", tag)], true);
		const rendered = render(view(base(false)));
		expect(screen.queryByText("Verified: the install succeeded.")).not.toBeInTheDocument();
		rendered.rerender(view(base(true)));
		expect(screen.getByText("Verified: the install succeeded.")).toBeInTheDocument();
	});

	it("is on screen in a long, windowed history", () => {
		const { turns, items } = manyEarlierTurns(tag);
		render(view(snapshot([
			...items,
			message("u", "ed", 900, "user", "human", "Is it installed?", tag),
			tell(901, tag),
			message("a", "ed", 902, "assistant", "provider", "Yes.", tag),
		], [...turns, turn("ed", 55, "completed", tag)])));
		expect(screen.getByText("Verified: the install succeeded.")).toBeInTheDocument();
	});
});

describe("a tell or ask inside an automation turn", () => {
	it("shows in the default view, where the rest of that turn is not delivered", () => {
		render(view(snapshot([
			message("u", "ed", 10, "user", "human", "Hello", undefined),
			message("a", "ed", 11, "assistant", "provider", "Hi.", undefined),
			tell(21),
		], [turn("ed", 5, "completed")])));
		expect(screen.getByText("Verified: the install succeeded.")).toBeInTheDocument();
	});

	it("shows in the internal view next to the internal turn it was sent from, unmarked", () => {
		render(view(snapshot([
			message("u", "ed", 10, "user", "human", "Hello", "human"),
			message("a", "ed", 11, "assistant", "provider", "Hi.", "human"),
			message("ping", "auto", 20, "user", "automation", "Phase wake: manager idle", "internal"),
			tell(21, "human"),
			message("ar", "auto", 22, "assistant", "provider", "Told Ed.", "internal"),
		], [turn("ed", 5, "completed", "human"), turn("auto", 15, "completed", "internal")])));
		expect(screen.getByText("Verified: the install succeeded.")).toBeInTheDocument();
		expect(screen.getByText(/Phase wake: manager idle/)).toBeInTheDocument();
		for (const region of document.querySelectorAll('[data-audience="internal"]')) {
			expect(within(region as HTMLElement).queryByText("Verified: the install succeeded.")).not.toBeInTheDocument();
		}
	});
});

describe("the From your coordinator block", () => {
	it("labels a tell and is never a bare line", () => {
		render(view(snapshot([message("u", "ed", 10, "user", "human", "Hi", "human"), tell(21, "human")], [turn("ed", 5, "completed", "human")])));
		const block = screen.getByRole("region", { name: "From your coordinator: Verified result" });
		expect(within(block).getByText("Verified: the install succeeded.")).toBeInTheDocument();
		expect(within(block).getByText("From your coordinator")).toBeInTheDocument();
		expect(within(block).queryByRole("button", { name: "Answer" })).not.toBeInTheDocument();
	});

	it("labels stopped work", () => {
		render(view(snapshot([{ ...tell(21), purpose: "stopped_work" } as ConversationItem], [])));
		expect(screen.getByRole("region", { name: "From your coordinator: Work stopped" })).toBeInTheDocument();
	});

	it("sits where it happened inside a completed Ed turn: visible, before the turn's final answer", () => {
		render(view(snapshot([
			message("u", "ed", 738, "user", "human", "it appears to have restarted"),
			message("a1", "ed", 739, "assistant", "provider", "You are on the new version."),
			tell(742),
			message("a2", "ed", 743, "assistant", "provider", "The send printed nothing."),
			message("a3", "ed", 745, "assistant", "provider", "The update worked."),
			message("u2", "ed2", 746, "user", "human", "Thanks"),
		], [turn("ed", 50, "completed"), turn("ed2", 55, "completed")])));
		const block = screen.getByRole("region", { name: "From your coordinator: Verified result" });
		const final = screen.getByText("The update worked.");
		const next = screen.getByText("Thanks");
		expect(block.compareDocumentPosition(final) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
		expect(final.compareDocumentPosition(next) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
		// It is not the turn's answer: only one message carries the copy action.
		expect(within(block).queryByRole("button", { name: /Copy message/ })).not.toBeInTheDocument();
	});

	it("between turns it is its own entry in sequence order", () => {
		render(view(snapshot([
			message("u", "ed", 10, "user", "human", "First"),
			message("a", "ed", 11, "assistant", "provider", "First answer."),
			tell(15),
			message("u2", "ed2", 20, "user", "human", "Second"),
			message("a2", "ed2", 21, "assistant", "provider", "Second answer."),
		], [turn("ed", 5, "completed"), turn("ed2", 15, "completed")])));
		const block = screen.getByRole("region", { name: /From your coordinator/ });
		expect(screen.getByText("First answer.").compareDocumentPosition(block) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
		expect(block.compareDocumentPosition(screen.getByText("Second")) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
	});

	it("scrolls into the transcript live while the turn runs, staying inside that turn", () => {
		const items = (withTell: boolean) => snapshot([
			message("u", "ed", 740, "user", "human", "Is it installed?"),
			message("a1", "ed", 741, "assistant", "provider", "Checking."),
			...(withTell ? [tell(742)] : []),
		], [turn("ed", 50, "running")], true);
		const rendered = render(view(items(false)));
		expect(screen.queryByRole("region", { name: /From your coordinator/ })).not.toBeInTheDocument();
		rendered.rerender(view(items(true)));
		expect(screen.getByRole("region", { name: "From your coordinator: Verified result" })).toBeInTheDocument();
	});

	it("keeps its place in a long paged page: tell inside the last, completed turn, older history still to load", () => {
		const { turns, items } = manyEarlierTurns("human");
		const snap = snapshot([
			...items.map((item) => ({ ...item, sequence: item.sequence + 290 })),
			message("u", "ed", 738, "user", "human", "it appears to have restarted", "human"),
			message("a1", "ed", 739, "assistant", "provider", "You are on the new version.", "human"),
			tell(742, "human"),
			message("a2", "ed", 743, "assistant", "provider", "The send printed nothing.", "human"),
			message("a3", "ed", 745, "assistant", "provider", "The update worked.", "human"),
		], [...turns, turn("ed", 50, "completed", "human")]);
		render(view({ ...snap, oldestSequence: 291, hasMoreBefore: true }));
		const block = screen.getByRole("region", { name: "From your coordinator: Verified result" });
		expect(block.compareDocumentPosition(screen.getByText("The update worked.")) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
		expect(screen.getByText("it appears to have restarted").compareDocumentPosition(block) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
	});
});

describe("jumping to what waits for Ed", () => {
	const items = [
		message("u", "ed", 10, "user", "human", "Hi", "human"),
		tell(21, "human"),
		message("a", "ed", 22, "assistant", "provider", "Done.", "human"),
	];
	const snap = () => snapshot(items, [turn("ed", 5, "completed", "human")]);
	const renderWith = (outOfView: string[], reportVisible = vi.fn(), unread: Partial<CoordinatorUnread> = {}) =>
		render(
			<TooltipProvider>
				<ChatWorkspace snapshot={snap()} coordinatorUnread={{ outOfView, reportVisible, older: { ids: [] }, settled: true, ...unread }} />
			</TooltipProvider>,
		);
	afterEach(() => {
		vi.unstubAllGlobals();
		useCoordinatorJumpStore.getState().clear();
	});

	it("shows a pill with the count while something waits outside the viewport, and none otherwise", () => {
		const rendered = renderWith(["tell"]);
		expect(screen.getByRole("button", { name: "1 message from your coordinator" })).toBeInTheDocument();
		rendered.unmount();
		renderWith([]);
		expect(screen.queryByRole("button", { name: /from your coordinator/ })).not.toBeInTheDocument();
	});

	it("pluralizes the pill", () => {
		renderWith(["tell", "decision-q1"]);
		expect(screen.getByRole("button", { name: "2 messages from your coordinator" })).toBeInTheDocument();
	});

	it("scrolls to the oldest and marks it for a moment", async () => {
		renderWith(["tell"]);
		await userEvent.click(screen.getByRole("button", { name: "1 message from your coordinator" }));
		const block = screen.getByRole("region", { name: "From your coordinator: Verified result" });
		await waitFor(() => expect(block).toHaveClass("ring-2"));
	});

	it("lands on it when the chat is opened from the coordinator row (a fresh request), once", async () => {
		useCoordinatorJumpStore.getState().request();
		renderWith(["tell"]);
		const block = screen.getByRole("region", { name: "From your coordinator: Verified result" });
		await waitFor(() => expect(block).toHaveClass("ring-2"));
		expect(useCoordinatorJumpStore.getState().requestedAt).toBeUndefined();
	});

	it("ignores a stale request", async () => {
		useCoordinatorJumpStore.setState({ requestedAt: Date.now() - 60_000 });
		renderWith(["tell"]);
		await new Promise((resolve) => setTimeout(resolve, 150));
		expect(screen.getByRole("region", { name: "From your coordinator: Verified result" })).not.toHaveClass("ring-2");
	});

	it("reports a block as seen only once it is in the viewport, and unseen when it leaves or unmounts", () => {
		let callback: IntersectionObserverCallback = () => undefined;
		const disconnect = vi.fn();
		vi.stubGlobal("IntersectionObserver", class {
			constructor(cb: IntersectionObserverCallback) { callback = cb; }
			observe() {}
			disconnect = disconnect;
		});
		const reportVisible = vi.fn();
		const rendered = renderWith(["tell"], reportVisible);
		expect(reportVisible).not.toHaveBeenCalledWith("tell", true);
		callback([{ intersectionRatio: 0.6 } as IntersectionObserverEntry], {} as IntersectionObserver);
		expect(reportVisible).toHaveBeenLastCalledWith("tell", true);
		callback([{ intersectionRatio: 0 } as IntersectionObserverEntry], {} as IntersectionObserver);
		expect(reportVisible).toHaveBeenLastCalledWith("tell", false);
		rendered.unmount();
		expect(disconnect).toHaveBeenCalled();
	});

	// (c) An unread tell or open ask OLDER than the loaded window keeps the Dock badge lit: the pill still
	// shows, says so, and loads earlier messages until it can jump to the oldest one.
	it("offers older waiting messages that are not loaded, and pages back to jump to the oldest", async () => {
		const onLoadOlder = vi.fn();
		const unread: CoordinatorUnread = { outOfView: [], reportVisible: vi.fn(), older: { ids: ["tell", "older-2"] }, settled: true };
		const without = snapshot([message("u2", "ed2", 30, "user", "human", "Later", "human")], [turn("ed2", 9, "completed", "human")]);
		const rendered = render(
			<TooltipProvider><ChatWorkspace snapshot={without} coordinatorUnread={unread} hasOlder onLoadOlder={onLoadOlder} /></TooltipProvider>,
		);
		const pill = screen.getByRole("button", { name: "2 older messages from your coordinator" });
		await userEvent.click(pill);
		await waitFor(() => expect(onLoadOlder).toHaveBeenCalled());
		// The earlier page arrives: the block is loaded and the jump lands on it.
		rendered.rerender(
			<TooltipProvider><ChatWorkspace snapshot={snap()} coordinatorUnread={{ ...unread, older: { ids: [] }, outOfView: ["tell"] }} hasOlder onLoadOlder={onLoadOlder} /></TooltipProvider>,
		);
		const block = await screen.findByRole("region", { name: "From your coordinator: Verified result" });
		await waitFor(() => expect(block).toHaveClass("ring-2"));
	});

	it("counts loaded and older waiting messages together", () => {
		renderWith(["tell"], vi.fn(), { older: { ids: ["decision-old", "older-2"] } });
		expect(screen.getByRole("button", { name: "3 messages from your coordinator" })).toBeInTheDocument();
	});

	it("stops offering a message a finished jump could not find", async () => {
		// Nothing older to load and the message is not in the transcript: the jump ends, the pill goes.
		renderWith([], vi.fn(), { older: { ids: ["gone"] } });
		await userEvent.click(screen.getByRole("button", { name: "1 older message from your coordinator" }));
		await waitFor(() => expect(screen.queryByRole("button", { name: /from your coordinator/ })).not.toBeInTheDocument());
	});

	// (a) A block taller than the viewport can never be a quarter in view.
	it("counts a block much taller than the viewport as seen once half a viewport of it shows", () => {
		let callback: IntersectionObserverCallback = () => undefined;
		const options: IntersectionObserverInit[] = [];
		vi.stubGlobal("IntersectionObserver", class {
			constructor(cb: IntersectionObserverCallback, init?: IntersectionObserverInit) { callback = cb; if (init) options.push(init); }
			observe() {}
			disconnect() {}
		});
		const reportVisible = vi.fn();
		renderWith(["tell"], reportVisible);
		// Observed against the chat scroller, with fine thresholds.
		expect(options[0]?.root).toBe(document.querySelector("[role=log]"));
		expect((options[0]?.threshold as number[]).length).toBeGreaterThan(10);
		const entry = (ratio: number, slice: number) => ({ intersectionRatio: ratio, intersectionRect: { height: slice }, rootBounds: { height: 400 } }) as IntersectionObserverEntry;
		callback([entry(0.02, 250)], {} as IntersectionObserver);
		expect(reportVisible).toHaveBeenLastCalledWith("tell", true);
		callback([entry(0.02, 100)], {} as IntersectionObserver);
		expect(reportVisible).toHaveBeenLastCalledWith("tell", false);
	});

	// (b) A row-click request that finds nothing waiting must not stay armed.
	it("spends a request that finds nothing waiting once everything has loaded", async () => {
		renderWith([]);
		useCoordinatorJumpStore.getState().request();
		await waitFor(() => expect(useCoordinatorJumpStore.getState().requestedAt).toBeUndefined());
	});

	it("keeps a request while the notifications that decide have not loaded", async () => {
		useCoordinatorJumpStore.getState().request();
		renderWith([], vi.fn(), { settled: false });
		await new Promise((resolve) => setTimeout(resolve, 100));
		expect(useCoordinatorJumpStore.getState().requestedAt).toBeDefined();
	});

	it("clears an unconsumed request when the chat is left after it loaded", async () => {
		useCoordinatorJumpStore.getState().request();
		// The waiting block is not in the transcript and there is nothing older to load: the jump gives up.
		const rendered = renderWith(["not-in-the-transcript"]);
		await new Promise((resolve) => setTimeout(resolve, 200));
		expect(useCoordinatorJumpStore.getState().requestedAt).toBeDefined();
		rendered.unmount();
		expect(useCoordinatorJumpStore.getState().requestedAt).toBeUndefined();
	});

	// (d) The arrow points at where the target is.
	it("points the pill's arrow at the target: up when it is above the viewport, down when below", async () => {
		const rect = (top: number) => ({ top, bottom: top + 100, height: 100, left: 0, right: 0, width: 0, x: 0, y: top, toJSON: () => ({}) }) as DOMRect;
		const spy = vi.spyOn(Element.prototype, "getBoundingClientRect");
		const place = (blockTop: number) =>
			spy.mockImplementation(function (this: Element) {
				if (this.getAttribute("role") === "log") return { ...rect(0), height: 400, bottom: 400 } as DOMRect;
				if (this.hasAttribute("data-coordinator-message")) return rect(blockTop);
				return rect(0);
			});
		const pillArrow = (direction: "up" | "down") =>
			screen.getByRole("button", { name: /from your coordinator/ }).querySelector(`svg.lucide-arrow-${direction}`);
		try {
			place(-300);
			const above = renderWith(["tell"]);
			await waitFor(() => expect(pillArrow("up")).not.toBeNull());
			expect(pillArrow("down")).toBeNull();
			above.unmount();
			place(900);
			renderWith(["tell"]);
			await waitFor(() => expect(pillArrow("down")).not.toBeNull());
			expect(pillArrow("up")).toBeNull();
		} finally {
			spy.mockRestore();
		}
	});
});

describe("answering a coordinator question", () => {
	const renderAsk = (options: { onSend?: ReturnType<typeof vi.fn>; askStatusOf?: () => { open: boolean; resolution?: "answered" | "coordinator" } | undefined } = {}) =>
		render(
			<TooltipProvider>
				<ChatWorkspace
					snapshot={snapshot([message("u", "ed", 10, "user", "human", "Hi"), ask(21)], [turn("ed", 5, "completed")])}
					onSend={options.onSend as never}
					askStatusOf={options.askStatusOf as never}
				/>
			</TooltipProvider>,
		);

	it("offers Answer on an open question, which names the question above the composer and sends replyToAskId", async () => {
		const onSend = vi.fn(async () => undefined);
		renderAsk({ onSend, askStatusOf: () => ({ open: true }) });
		await userEvent.click(screen.getByRole("button", { name: "Answer" }));
		expect(screen.getByTestId("answering-chip")).toHaveTextContent("Answering: Which migration order should we use?");
		await typeInLexicalEditor(screen.getByRole("combobox", { name: "Message the agent" }), "Billing first");
		await userEvent.click(screen.getByRole("button", { name: "Send message" }));
		await waitFor(() => expect(onSend).toHaveBeenCalled());
		const call = onSend.mock.calls[0] as unknown[];
		expect(call[0]).toBe("Billing first");
		expect(call[4]).toBe("decision-q1");
		await waitFor(() => expect(screen.queryByTestId("answering-chip")).not.toBeInTheDocument());
	});

	it("a message sent without choosing Answer carries no reply, and the chip can be dismissed", async () => {
		const onSend = vi.fn(async () => undefined);
		renderAsk({ onSend, askStatusOf: () => ({ open: true }) });
		await userEvent.click(screen.getByRole("button", { name: "Answer" }));
		await userEvent.click(screen.getByRole("button", { name: "Stop answering this question" }));
		expect(screen.queryByTestId("answering-chip")).not.toBeInTheDocument();
		await typeInLexicalEditor(screen.getByRole("combobox", { name: "Message the agent" }), "Unrelated");
		await userEvent.click(screen.getByRole("button", { name: "Send message" }));
		await waitFor(() => expect(onSend).toHaveBeenCalled());
		expect((onSend.mock.calls[0] as unknown[])[4]).toBeUndefined();
	});

	it("shows an answered question as settled, with no Answer button", () => {
		renderAsk({ askStatusOf: () => ({ open: false, resolution: "answered" }) });
		const block = screen.getByRole("region", { name: "From your coordinator: Needs your decision" });
		expect(within(block).getByText("Answered")).toBeInTheDocument();
		expect(within(block).queryByRole("button", { name: "Answer" })).not.toBeInTheDocument();
	});

	it("says when the coordinator resolved it", () => {
		renderAsk({ askStatusOf: () => ({ open: false, resolution: "coordinator" }) });
		expect(screen.getByText("Resolved by the coordinator")).toBeInTheDocument();
	});

	it("drops the answering chip once the question is settled elsewhere", async () => {
		let open = true;
		const rendered = renderAsk({ askStatusOf: () => ({ open }) });
		await userEvent.click(screen.getByRole("button", { name: "Answer" }));
		expect(screen.getByTestId("answering-chip")).toBeInTheDocument();
		open = false;
		rendered.rerender(
			<TooltipProvider>
				<ChatWorkspace snapshot={snapshot([message("u", "ed", 10, "user", "human", "Hi"), ask(21)], [turn("ed", 5, "completed")])} askStatusOf={(() => ({ open })) as never} />
			</TooltipProvider>,
		);
		await waitFor(() => expect(screen.queryByTestId("answering-chip")).not.toBeInTheDocument());
	});
});
