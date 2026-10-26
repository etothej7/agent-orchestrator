import { render, screen, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
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
const message = (id: string, turnId: string | undefined, sequence: number, role: "user" | "assistant", origin: "human" | "automation" | "daemon" | "provider", text: string, audience?: ConversationAudienceTag): ConversationItem => ({
	kind: "message", id, turnId, sequence, revision: 1, role, origin, text, streaming: false, audience, createdAt: at(sequence),
});
const tell = (sequence: number, audience?: ConversationAudienceTag) => message("tell", undefined, sequence, "assistant", "daemon", "Verified: the install succeeded.", audience);

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
