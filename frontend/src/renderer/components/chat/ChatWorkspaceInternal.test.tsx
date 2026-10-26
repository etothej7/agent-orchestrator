import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import { chatFixtureEmpty } from "../../lib/chat-fixture";
import type { ConversationAudienceTag, ConversationItem, ConversationSnapshot, ConversationTurn } from "../../types/conversation";
import { TooltipProvider } from "../ui/tooltip";
import { ChatWorkspace } from "./ChatWorkspace";

const at = (minute: number) => `2026-10-25T10:${String(minute).padStart(2, "0")}:00Z`;
const turn = (id: string, minute: number): ConversationTurn => ({
	id,
	state: "completed",
	requestedAt: at(minute),
	startedAt: at(minute),
	completedAt: at(minute + 1),
	hasHumanPrompt: id === "ed-turn",
});
const message = (id: string, turnId: string, sequence: number, role: "user" | "assistant", origin: "human" | "automation" | "provider", text: string, audience?: ConversationAudienceTag): ConversationItem => ({
	kind: "message",
	id,
	turnId,
	sequence,
	revision: 1,
	role,
	origin,
	text,
	streaming: false,
	editAvailable: role === "user" && origin === "human",
	audience,
	createdAt: at(sequence),
});
const activity = (id: string, turnId: string, sequence: number, audience?: ConversationAudienceTag): ConversationItem => ({
	kind: "activity",
	id,
	turnId,
	sequence,
	revision: 1,
	activityKind: "command",
	status: "completed",
	summary: `Ran ${id}`,
	audience,
	createdAt: at(sequence),
});

/** Ed's turn (with one hidden tool run inside it) followed by an automation turn. */
function snapshot(audiences: { human?: ConversationAudienceTag; internal?: ConversationAudienceTag }): ConversationSnapshot {
	return {
		...chatFixtureEmpty,
		controller: { state: "ready" },
		capabilities: [],
		latestSequence: 8,
		oldestSequence: 1,
		hasMoreBefore: false,
		turns: [turn("ed-turn", 1), turn("auto-turn", 5)],
		items: [
			message("ed-ask", "ed-turn", 1, "user", "human", "Status of the migration?", audiences.human),
			activity("ed-tool", "ed-turn", 2, audiences.internal),
			message("ed-answer", "ed-turn", 3, "assistant", "provider", "Migration is on track.", audiences.human),
			message("auto-ping", "auto-turn", 5, "user", "automation", "Phase wake: manager idle", audiences.internal),
			message("auto-reply", "auto-turn", 6, "assistant", "provider", "Nudged the manager.", audiences.internal),
		],
	};
}

function renderChat(snap: ConversationSnapshot, onEditMessage = vi.fn(async () => undefined)) {
	render(<TooltipProvider><ChatWorkspace snapshot={snap} onEditMessage={onEditMessage} /></TooltipProvider>);
	return onEditMessage;
}
const internalRegions = () => Array.from(document.querySelectorAll<HTMLElement>('[data-audience="internal"]'));

describe("coordinator's unfiltered view", () => {
	it("marks automation turns as internal while Ed's own turn stays unmarked", () => {
		renderChat(snapshot({ human: "human", internal: "internal" }));
		const [turnRegion] = internalRegions();
		expect(turnRegion).toBeDefined();
		expect(within(turnRegion!).getByText("Internal activity")).toBeInTheDocument();
		expect(within(turnRegion!).getByText("Nudged the manager.")).toBeInTheDocument();
		expect(within(turnRegion!).getByText(/Phase wake: manager idle/)).toBeInTheDocument();
		for (const region of internalRegions()) {
			expect(within(region).queryByText("Migration is on track.")).not.toBeInTheDocument();
			expect(within(region).queryByText("Status of the migration?")).not.toBeInTheDocument();
		}
		expect(screen.getByText("Migration is on track.")).toBeInTheDocument();
	});

	it("marks hidden tool activity inside a turn Ed started without marking the turn itself", async () => {
		renderChat(snapshot({ human: "human", internal: "internal" }));
		// A settled turn folds its tool runs under "Worked for"; the mark shows once opened.
		await userEvent.click(await screen.findByRole("button", { name: /Worked for/ }));
		const regions = internalRegions();
		const toolRegion = regions.find((region) => !region.textContent?.includes("Phase wake"));
		expect(toolRegion).toBeDefined();
		expect(toolRegion).not.toContainElement(screen.getByText("Migration is on track."));
		expect(toolRegion).not.toContainElement(screen.getByText("Status of the migration?"));
	});

	it("leaves nothing editable behind: Ed's message keeps its edit action", async () => {
		const onEdit = renderChat(snapshot({ human: "human", internal: "internal" }));
		const edit = screen.getByRole("button", { name: "Edit user message" });
		expect(internalRegions().every((region) => !region.contains(edit))).toBe(true);
		await userEvent.click(edit);
		await userEvent.click(await screen.findByRole("button", { name: "Send edited message" }));
		expect(onEdit).toHaveBeenCalledWith("ed-turn", "Status of the migration?", expect.any(String));
	});

	it("names every internal group for assistive tech but shows one visible label per run of consecutive internal turns", () => {
		const base = snapshot({ human: "human", internal: "internal" });
		renderChat({
			...base,
			latestSequence: 12,
			turns: [...base.turns, turn("auto-turn-2", 9)],
			items: [
				...base.items,
				message("auto-ping-2", "auto-turn-2", 9, "user", "automation", "CI event: checks passed", "internal"),
				message("auto-reply-2", "auto-turn-2", 10, "assistant", "provider", "Recorded.", "internal"),
			],
		});
		const groups = screen.getAllByRole("group", { name: "Internal activity" });
		// Ed's turn holds one internal tool run (own label); the two automation turns form one run.
		const turnGroups = groups.filter((group) => group.textContent?.includes("Phase wake") || group.textContent?.includes("CI event"));
		expect(turnGroups).toHaveLength(2);
		expect(screen.getAllByText("Internal activity", { selector: "span" }).filter((label) => turnGroups.some((group) => group.contains(label)))).toHaveLength(1);
	});

	it("shows no internal marking in the default view", () => {
		renderChat(snapshot({}));
		expect(internalRegions()).toHaveLength(0);
		expect(screen.queryByText("Internal activity")).not.toBeInTheDocument();
	});
});
