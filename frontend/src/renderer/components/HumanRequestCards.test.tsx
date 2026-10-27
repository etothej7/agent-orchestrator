import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { humanRequestsQueryKey, type HumanRequestDTO } from "../hooks/useHumanRequests";
import { sessionAutoResumeQueryKey } from "../hooks/useSessionAutoResume";
import { apiClient } from "../lib/api-client";
import { useUiStore } from "../stores/ui-store";
import { HumanRequestCards } from "./HumanRequestCards";

function request(overrides: Partial<HumanRequestDTO> = {}): HumanRequestDTO {
	return {
		id: "hr-1",
		kind: "conversation_settings",
		sessionId: "repo-1",
		requester: { kind: "session", sessionId: "repo-2" },
		summary: "Change Fix the bug (repo-1): model to m-b, reasoning effort to medium.",
		changes: [
			{ field: "model", from: "m-a", to: "m-b" },
			{ field: "reasoningEffort", from: "low", to: "medium" },
		],
		payloadHash: "hash-1",
		state: "pending",
		createdAt: "2026-10-27T12:00:00Z",
		expiresAt: "2026-10-28T12:00:00Z",
		...overrides,
	};
}

const lift = () =>
	request({
		id: "hr-2",
		kind: "auto_resume_lift",
		requester: { kind: "terminal" },
		summary: "Allow automatic resume for Fix the bug (repo-1). It is protected today.",
		changes: [{ field: "autoResume", from: "never", to: "allowed" }],
		payloadHash: "hash-2",
	});

let client: QueryClient;

function mount(requests: HumanRequestDTO[]) {
	client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
	return render(
		<QueryClientProvider client={client}>
			<HumanRequestCards requests={requests} />
		</QueryClientProvider>,
	);
}

const post = (result: object = {}) =>
	vi.spyOn(apiClient, "POST").mockResolvedValue({ data: { request: request({ state: "applied" }) }, response: { status: 200 }, ...result } as never);

beforeEach(() => useUiStore.setState({ globalToasts: [], globalToast: null }));
afterEach(() => vi.restoreAllMocks());

describe("HumanRequestCards", () => {
	it("renders nothing when no request is waiting", () => {
		const { container } = mount([]);
		expect(container).toBeEmptyDOMElement();
	});

	it("shows what the daemon rendered, who asked, and the before and after of each change", () => {
		mount([request(), lift()]);
		const section = screen.getByRole("region", { name: "Requests waiting for your approval" });
		expect(within(section).getByText("2 requests need your approval")).toBeInTheDocument();
		const first = section.querySelector('[data-request-id="hr-1"]') as HTMLElement;
		expect(within(first).getByText("Change model or settings")).toBeInTheDocument();
		expect(within(first).getByText("Claims to be repo-2 (not verified)")).toBeInTheDocument();
		expect(within(first).getByText("Change Fix the bug (repo-1): model to m-b, reasoning effort to medium.")).toBeInTheDocument();
		expect(within(first).getByText("Model")).toBeInTheDocument();
		expect(within(first).getByText("m-a → m-b")).toBeInTheDocument();
		expect(within(first).getByText("Reasoning effort")).toBeInTheDocument();
		expect(within(first).getByText("low → medium")).toBeInTheDocument();
		const second = section.querySelector('[data-request-id="hr-2"]') as HTMLElement;
		expect(within(second).getByText("Requested from a terminal, not identified")).toBeInTheDocument();
		expect(within(second).getByText("never → allowed")).toBeInTheDocument();
	});

	it("uses the singular for one request and falls back to the raw kind and field for ones it does not know", () => {
		mount([request({ kind: "future_kind", changes: [{ field: "colour", from: "", to: "blue" }] })]);
		expect(screen.getByText("1 request needs your approval")).toBeInTheDocument();
		expect(screen.getByText("future_kind")).toBeInTheDocument();
		expect(screen.getByText("colour")).toBeInTheDocument();
		expect(screen.getByText("blue")).toBeInTheDocument();
	});

	it("does not mistake an inherited property name for a known kind or field", () => {
		mount([request({ kind: "toString", changes: [{ field: "constructor", from: "", to: "x" }] })]);
		expect(screen.getByText("toString")).toBeInTheDocument();
		expect(screen.getByText("constructor")).toBeInTheDocument();
	});

	it("shows the daemon's text as plain text, whatever it contains", () => {
		const { container } = mount([request({ summary: "<b>fix the build</b> <img src=x onerror=alert(1)>" })]);
		expect(screen.getByText("<b>fix the build</b> <img src=x onerror=alert(1)>")).toBeInTheDocument();
		expect(container.querySelector("b")).toBeNull();
		expect(container.querySelector("img")).toBeNull();
	});

	it("approves a settings request with one click, naming the hash that was shown", async () => {
		const spy = post();
		mount([request()]);
		client.setQueryData(humanRequestsQueryKey, [request()]);
		const invalidate = vi.spyOn(client, "invalidateQueries");
		await userEvent.click(screen.getByRole("button", { name: "Approve: Change model or settings" }));
		await vi.waitFor(() => expect(spy).toHaveBeenCalledTimes(1));
		expect(spy).toHaveBeenCalledWith("/api/v1/human-requests/{id}/approve", { params: { path: { id: "hr-1" } }, body: { payloadHash: "hash-1" } });
		await vi.waitFor(() => expect(invalidate).toHaveBeenCalledWith({ queryKey: humanRequestsQueryKey }));
		expect(invalidate).toHaveBeenCalledWith({ queryKey: sessionAutoResumeQueryKey("repo-1") });
		await vi.waitFor(() => expect(useUiStore.getState().globalToasts.map((toast) => toast.title)).toContain("Approved"));
	});

	it("denies with one click and sends no hash", async () => {
		const spy = post();
		mount([request()]);
		await userEvent.click(screen.getByRole("button", { name: "Deny: Change model or settings" }));
		await vi.waitFor(() => expect(spy).toHaveBeenCalledTimes(1));
		expect(spy).toHaveBeenCalledWith("/api/v1/human-requests/{id}/deny", { params: { path: { id: "hr-1" } } });
		await vi.waitFor(() => expect(useUiStore.getState().globalToasts.map((toast) => toast.title)).toContain("Denied"));
	});

	it("asks once more before lifting a protection, opens on Cancel, and a repeated Enter confirms nothing", async () => {
		const spy = post();
		mount([lift()]);
		await userEvent.click(screen.getByRole("button", { name: "Approve: Allow automatic resume" }));
		const dialog = await screen.findByRole("dialog", { name: "Allow automatic resume?" });
		expect(dialog).toHaveTextContent("Allow automatic resume for Fix the bug (repo-1). It is protected today.");
		expect(spy).not.toHaveBeenCalled();
		await vi.waitFor(() => expect(screen.getByRole("button", { name: "Cancel" })).toHaveFocus());
		await userEvent.keyboard("{Enter}");
		expect(spy).not.toHaveBeenCalled();
		expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
	});

	it("approves a lift only from the dialog's own confirm", async () => {
		const spy = post();
		mount([lift()]);
		await userEvent.click(screen.getByRole("button", { name: "Approve: Allow automatic resume" }));
		await userEvent.click(await screen.findByRole("button", { name: "Allow" }));
		await vi.waitFor(() => expect(spy).toHaveBeenCalledTimes(1));
		expect(spy).toHaveBeenCalledWith("/api/v1/human-requests/{id}/approve", { params: { path: { id: "hr-2" } }, body: { payloadHash: "hash-2" } });
		await vi.waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
	});

	it("keeps the card and shows the daemon's refusal when approving fails", async () => {
		post({ data: undefined, error: { message: "this request is not the one that was shown" }, response: { status: 409 } });
		mount([request()]);
		await userEvent.click(screen.getByRole("button", { name: "Approve: Change model or settings" }));
		expect(await screen.findByRole("alert")).toHaveTextContent("this request is not the one that was shown");
		expect(screen.getByRole("button", { name: "Approve: Change model or settings" })).toBeEnabled();
		expect(useUiStore.getState().globalToasts).toHaveLength(0);
	});

	it("keeps the lift dialog open and shows the refusal when the lift fails", async () => {
		post({ data: undefined, error: { message: "this request expired" }, response: { status: 409 } });
		mount([lift()]);
		await userEvent.click(screen.getByRole("button", { name: "Approve: Allow automatic resume" }));
		await userEvent.click(await screen.findByRole("button", { name: "Allow" }));
		const alerts = await screen.findAllByRole("alert");
		expect(alerts.some((alert) => alert.textContent?.includes("this request expired"))).toBe(true);
		expect(screen.getByRole("dialog")).toBeInTheDocument();
	});
});
