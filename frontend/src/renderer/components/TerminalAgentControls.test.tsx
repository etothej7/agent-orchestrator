import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, expect, it } from "vitest";
import { useConversationAudienceStore } from "../stores/conversation-audience-store";
import type { WorkspaceSession } from "../types/workspace";
import { SessionTabMenu } from "./TerminalAgentControls";
import { TooltipProvider } from "./ui/tooltip";

const coordinator = { id: "coord-1", workspaceId: "overall", kind: "orchestrator", title: "Main" } as WorkspaceSession;
const manager = { id: "mgr-1", workspaceId: "repo", kind: "orchestrator", title: "Manager" } as WorkspaceSession;

beforeEach(() => useConversationAudienceStore.setState({ showInternal: {} }));

function mount(session: WorkspaceSession, props: { hostId?: string; switchTarget?: "chat" | "tui" } = {}) {
	const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
	client.setQueryData(["coordination"], { coordinatorProjectId: "overall" });
	return render(
		<QueryClientProvider client={client}>
			<TooltipProvider>
				<SessionTabMenu session={session} hostId={props.hostId} blocked={false} switchTarget={props.switchTarget ?? "tui"} />
			</TooltipProvider>
		</QueryClientProvider>,
	);
}

it("keeps Show internal activity in the main coordinator's session menu, off until chosen", async () => {
	mount(coordinator);
	await userEvent.click(screen.getByRole("button", { name: "Session actions" }));
	const item = await screen.findByRole("menuitemcheckbox", { name: "Show internal activity" });
	expect(item).toHaveAttribute("aria-checked", "false");
	await userEvent.click(item);
	expect(useConversationAudienceStore.getState().showInternal).toEqual({ "coord-1": true });

	await userEvent.click(screen.getByRole("button", { name: "Session actions" }));
	const again = await screen.findByRole("menuitemcheckbox", { name: "Show internal activity" });
	expect(again).toHaveAttribute("aria-checked", "true");
	await userEvent.click(again);
	expect(useConversationAudienceStore.getState().showInternal).toEqual({});
});

it("offers no such choice on other conversations, in a terminal, or for a remote host", () => {
	mount(manager);
	expect(screen.queryByRole("button", { name: "Session actions" })).not.toBeInTheDocument();
});

it("is absent for the coordinator when the conversation is not the one on screen", () => {
	mount(coordinator, { switchTarget: "chat" });
	expect(screen.queryByRole("button", { name: "Session actions" })).not.toBeInTheDocument();
});

it("is absent for a remote host's session", () => {
	mount(coordinator, { hostId: "box-a" });
	expect(screen.queryByRole("button", { name: "Session actions" })).not.toBeInTheDocument();
});
