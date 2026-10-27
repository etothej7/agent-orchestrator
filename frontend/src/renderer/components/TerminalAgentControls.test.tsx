import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { apiClient, setApiBaseUrl } from "../lib/api-client";
import { sessionAutoResumeQueryKey } from "../hooks/useSessionAutoResume";
import { useUiStore } from "../stores/ui-store";
import { useConversationAudienceStore } from "../stores/conversation-audience-store";
import type { WorkspaceSession } from "../types/workspace";
import { SessionTabMenu } from "./TerminalAgentControls";
import { TooltipProvider } from "./ui/tooltip";

let mountedClient: QueryClient;
const worker = { id: "w-1", workspaceId: "repo", kind: "worker", title: "Fix the bug" } as WorkspaceSession;
const coordinator = { id: "coord-1", workspaceId: "overall", kind: "orchestrator", title: "Main" } as WorkspaceSession;
const manager = { id: "mgr-1", workspaceId: "repo", kind: "orchestrator", title: "Manager" } as WorkspaceSession;

beforeEach(() => {
	useConversationAudienceStore.setState({ showInternal: {} });
	useUiStore.setState({ globalToasts: [], globalToast: null });
});
afterEach(() => vi.restoreAllMocks());

function mount(session: WorkspaceSession, props: { hostId?: string; switchTarget?: "chat" | "tui"; autoResume?: "allowed" | "never" } = {}) {
	const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
	client.setQueryData(["coordination"], { coordinatorProjectId: "overall" });
	if (props.autoResume) client.setQueryData(sessionAutoResumeQueryKey(session.id), props.autoResume);
	mountedClient = client;
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

const putAutoResume = (result: { error?: unknown; response?: { status: number } } = {}) =>
	vi.spyOn(apiClient, "PUT").mockResolvedValue({ data: { sessionId: "w-1", policy: "allowed" }, response: { status: 200 }, ...result } as never);

it("offers Allow automatic resume only while the session's policy is never", async () => {
	mount(worker, { autoResume: "never" });
	await userEvent.click(screen.getByRole("button", { name: "Session actions" }));
	expect(await screen.findByRole("menuitem", { name: "Allow automatic resume…" })).toBeInTheDocument();
});

it("does not offer it when the policy is allowed", () => {
	mount(worker, { autoResume: "allowed" });
	expect(screen.queryByRole("button", { name: "Session actions" })).not.toBeInTheDocument();
});

it("does not offer it for a remote host's session even when it reads never", () => {
	mount(worker, { autoResume: "never", hostId: "box-a" });
	expect(screen.queryByRole("button", { name: "Session actions" })).not.toBeInTheDocument();
});

it("does not offer it before the policy is known", () => {
	mount(worker);
	expect(screen.queryByRole("button", { name: "Session actions" })).not.toBeInTheDocument();
});

it("opens on Cancel, so a repeated Enter or Space cannot confirm, and shows the session id beside the title", async () => {
	const put = putAutoResume();
	mount(worker, { autoResume: "never" });
	await userEvent.click(screen.getByRole("button", { name: "Session actions" }));
	await userEvent.click(await screen.findByRole("menuitem", { name: "Allow automatic resume…" }));
	const dialog = await screen.findByRole("dialog", { name: "Allow automatic resume for Fix the bug?" });
	expect(dialog).toHaveTextContent("Session ID: w-1");
	await vi.waitFor(() => expect(screen.getByRole("button", { name: "Cancel" })).toHaveFocus());
	await userEvent.keyboard("{Enter}");
	expect(put).not.toHaveBeenCalled();
	expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
});

it("reads the policy once for several menus and does not refetch on window focus", async () => {
	setApiBaseUrl("http://127.0.0.1:9");
	try {
		const get = vi.spyOn(apiClient, "GET").mockResolvedValue({ data: { sessionId: "w-1", policy: "never" }, response: { status: 200 } } as never);
		const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
		const view = (
			<QueryClientProvider client={client}>
				<TooltipProvider>
					<SessionTabMenu session={worker} blocked={false} switchTarget="tui" />
				</TooltipProvider>
			</QueryClientProvider>
		);
		const first = render(view);
		await screen.findByRole("button", { name: "Session actions" });
		first.unmount();
		render(view);
		await screen.findByRole("button", { name: "Session actions" });
		window.dispatchEvent(new Event("focus"));
		expect(get).toHaveBeenCalledTimes(1);
	} finally {
		setApiBaseUrl(null);
	}
});

it("asks first, names the session and what allowing means, and sends nothing on cancel", async () => {
	const put = putAutoResume();
	mount(worker, { autoResume: "never" });
	await userEvent.click(screen.getByRole("button", { name: "Session actions" }));
	await userEvent.click(await screen.findByRole("menuitem", { name: "Allow automatic resume…" }));
	const dialog = await screen.findByRole("dialog", { name: "Allow automatic resume for Fix the bug?" });
	expect(dialog).toHaveTextContent("AO may resume it after a restart, replace it, and tear it down on its own again");
	expect(put).not.toHaveBeenCalled();
	await userEvent.click(screen.getByRole("button", { name: "Cancel" }));
	expect(put).not.toHaveBeenCalled();
	expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
});

it("confirming sends policy allowed with confirmedInApp, refreshes the policy and says so", async () => {
	const put = putAutoResume();
	mount(worker, { autoResume: "never" });
	const invalidate = vi.spyOn(mountedClient, "invalidateQueries");
	await userEvent.click(screen.getByRole("button", { name: "Session actions" }));
	await userEvent.click(await screen.findByRole("menuitem", { name: "Allow automatic resume…" }));
	await userEvent.click(await screen.findByRole("button", { name: "Allow" }));
	await vi.waitFor(() => expect(put).toHaveBeenCalledTimes(1));
	expect(put).toHaveBeenCalledWith("/api/v1/sessions/{sessionId}/auto-resume", {
		params: { path: { sessionId: "w-1" } },
		body: { policy: "allowed", confirmedInApp: true },
	});
	await vi.waitFor(() => expect(invalidate).toHaveBeenCalledWith({ queryKey: sessionAutoResumeQueryKey("w-1") }));
	await vi.waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
	expect(useUiStore.getState().globalToasts.map((toast) => toast.title)).toContain("Automatic resume allowed");
});

it("keeps the dialog open and shows the daemon's refusal when the request fails", async () => {
	putAutoResume({ error: { message: "nope" }, response: { status: 403 } });
	mount(worker, { autoResume: "never" });
	await userEvent.click(screen.getByRole("button", { name: "Session actions" }));
	await userEvent.click(await screen.findByRole("menuitem", { name: "Allow automatic resume…" }));
	await userEvent.click(await screen.findByRole("button", { name: "Allow" }));
	expect(await screen.findByRole("alert")).toBeInTheDocument();
	expect(screen.getByRole("dialog")).toBeInTheDocument();
	expect(useUiStore.getState().globalToasts).toHaveLength(0);
});
