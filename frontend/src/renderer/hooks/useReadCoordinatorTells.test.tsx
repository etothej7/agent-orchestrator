import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import type { NotificationDTO } from "../lib/notifications";
import type { ConversationItem } from "../types/conversation";

const { markMock, getMock } = vi.hoisted(() => ({ markMock: vi.fn(), getMock: vi.fn() }));
vi.mock("../lib/notifications", async (importOriginal) => ({
	...(await importOriginal<typeof import("../lib/notifications")>()),
	markAllNotificationsRead: markMock,
}));
vi.mock("../lib/api-client", async (importOriginal) => ({
	...(await importOriginal<typeof import("../lib/api-client")>()),
	apiClient: { GET: (...args: unknown[]) => { getMock(...args); return new Promise(() => undefined); } },
}));

import { useReadCoordinatorTells } from "./useReadCoordinatorTells";

const tellMessage = (id: string, purpose: string, text: string): ConversationItem => ({
	kind: "message", id, sequence: 1, revision: 1, role: "assistant", origin: "daemon", streaming: false, text, purpose, createdAt: "2026-10-26T12:00:00Z",
});
const note = (id: string, purpose: NonNullable<NotificationDTO["purpose"]>, body: string, status: "read" | "unread" = "unread", messageId = id.replace("ntf_", "")): NotificationDTO => ({
	id, sessionId: "coord", projectId: "overall", prUrl: "", type: "coordinator_message", purpose, messageId, title: id, body, status,
	createdAt: "2026-10-26T12:00:01Z", target: { kind: "session", sessionId: "coord" },
});

function mount(options: { enabled?: boolean; onScreen?: boolean; items: ConversationItem[]; notifications: NotificationDTO[]; inView?: string[]; askStatusOf?: Parameters<typeof useReadCoordinatorTells>[3]; openAsks?: NotificationDTO[]; sessionId?: string }) {
	const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
	const openCount = options.openAsks?.length ?? 0;
	client.setQueryData(["notifications", "history", "all"], { pageParams: [""], pages: [{ notifications: options.notifications, unreadCount: 0, unresolvedCount: 0, unresolvedAskCount: openCount }] });
	if (options.openAsks) client.setQueryData(["coordinator-open-asks", openCount], options.openAsks);
	const wrapper = ({ children }: { children: ReactNode }) => <QueryClientProvider client={client}>{children}</QueryClientProvider>;
	const rendered = renderHook(() => useReadCoordinatorTells(options.enabled ?? true, options.onScreen ?? true, options.items, options.askStatusOf, options.sessionId ?? "coord"), { wrapper });
	// By default every message is in the viewport; a test passes `inView` to say otherwise.
	act(() => {
		for (const id of options.inView ?? options.items.map((item) => item.id)) rendered.result.current.reportVisible(id, true);
	});
	return rendered;
}

let focused = true;
beforeEach(() => {
	markMock.mockReset().mockResolvedValue(1);
	focused = true;
	vi.spyOn(document, "hasFocus").mockImplementation(() => focused);
	vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
});
afterEach(() => vi.restoreAllMocks());

it("marks an unread result read once it is in the focused coordinator chat", async () => {
	mount({ items: [tellMessage("tell", "verified_result", "Verified: it works.")], notifications: [note("ntf_tell", "verified_result", "Verified: it works.")] });
	await waitFor(() => expect(markMock).toHaveBeenCalled());
	expect(markMock.mock.calls[0]?.[0]).toEqual(["ntf_tell"]);
	expect(markMock).toHaveBeenCalledTimes(1);
});

it("marks stopped work read too", async () => {
	mount({ items: [tellMessage("stop", "stopped_work", "Work stopped.")], notifications: [note("ntf_stop", "stopped_work", "Work stopped.")] });
	await waitFor(() => expect(markMock).toHaveBeenCalled());
	expect(markMock.mock.calls[0]?.[0]).toEqual(["ntf_stop"]);
});

it("does nothing while the chat is not on screen, the window is not focused, or it is not the coordinator", async () => {
	const base = { items: [tellMessage("tell", "verified_result", "Verified: it works.")], notifications: [note("ntf_tell", "verified_result", "Verified: it works.")] };
	mount({ ...base, onScreen: false });
	focused = false;
	mount(base);
	mount({ ...base, enabled: false });
	await new Promise((resolve) => setTimeout(resolve, 50));
	expect(markMock).not.toHaveBeenCalled();
});

it("never marks an open question: only answering it clears it", async () => {
	mount({ items: [tellMessage("decision-q", "blocking_decision", "Which order?")], notifications: [note("ntf_q", "blocking_decision", "Which order?")] });
	await new Promise((resolve) => setTimeout(resolve, 50));
	expect(markMock).not.toHaveBeenCalled();
});

it("leaves a result alone until its message is actually in the chat", async () => {
	mount({ items: [], notifications: [note("ntf_tell", "verified_result", "Verified: it works.")] });
	await new Promise((resolve) => setTimeout(resolve, 50));
	expect(markMock).not.toHaveBeenCalled();
});

it("skips results that were already read", async () => {
	mount({ items: [tellMessage("tell", "verified_result", "Verified: it works.")], notifications: [note("ntf_tell", "verified_result", "Verified: it works.", "read")] });
	await new Promise((resolve) => setTimeout(resolve, 50));
	expect(markMock).not.toHaveBeenCalled();
});

it("does not mark a result read until its block has entered the viewport", async () => {
	const { result } = mount({
		items: [tellMessage("tell", "verified_result", "Verified: it works.")],
		notifications: [note("ntf_tell", "verified_result", "Verified: it works.")],
		inView: [],
	});
	await new Promise((resolve) => setTimeout(resolve, 50));
	expect(markMock).not.toHaveBeenCalled();
	expect(result.current.outOfView).toEqual(["tell"]);
	act(() => result.current.reportVisible("tell", true));
	await waitFor(() => expect(markMock).toHaveBeenCalledTimes(1));
	expect(markMock.mock.calls[0]?.[0]).toEqual(["ntf_tell"]);
	expect(result.current.outOfView).toEqual([]);
});

it("lists unread results and open questions outside the viewport, oldest first, and never a settled question", () => {
	const items = [
		{ ...tellMessage("ask-old", "blocking_decision", "Which order?"), sequence: 1 },
		{ ...tellMessage("tell", "verified_result", "Verified: it works."), sequence: 2 },
		{ ...tellMessage("ask-done", "blocking_decision", "Already answered?"), sequence: 3 },
	];
	const { result } = mount({
		items,
		notifications: [note("ntf_tell", "verified_result", "Verified: it works.")],
		inView: [],
		askStatusOf: (message) => ({ open: message.id !== "ask-done" }),
	});
	expect(result.current.outOfView).toEqual(["ask-old", "tell"]);
});

// An unread tell or open ask whose message is older than the loaded window keeps the Dock badge lit, so the
// chat must know about it even though its block is not in the loaded pages.
it("counts unread results and open questions older than the loaded window, oldest first", () => {
	const older = (id: string, purpose: NonNullable<NotificationDTO["purpose"]>, at: string): NotificationDTO => ({ ...note(`ntf_${id}`, purpose, "text"), messageId: id, createdAt: at });
	const { result } = mount({
		items: [tellMessage("loaded-tell", "verified_result", "Verified: it works.")],
		notifications: [
			older("old-tell", "verified_result", "2026-10-26T09:00:00Z"),
			older("loaded-tell", "verified_result", "2026-10-26T12:00:00Z"),
			older("read-tell", "verified_result", "2026-10-26T08:00:00Z"),
		].map((n) => (n.messageId === "read-tell" ? { ...n, status: "read" as const } : n)),
		openAsks: [older("decision-old", "blocking_decision", "2026-10-26T07:00:00Z")],
	});
	expect(result.current.older).toEqual({ ids: ["decision-old", "old-tell"] });
	expect(result.current.settled).toBe(true);
});

it("has nothing older when everything that waits is in the loaded window, and is not settled before notifications load", () => {
	const loaded = mount({ items: [tellMessage("tell", "verified_result", "Verified: it works.")], notifications: [note("ntf_tell", "verified_result", "Verified: it works.")], openAsks: [] });
	expect(loaded.result.current.older).toEqual({ ids: [] });
	const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
	const wrapper = ({ children }: { children: ReactNode }) => <QueryClientProvider client={client}>{children}</QueryClientProvider>;
	const loading = renderHook(() => useReadCoordinatorTells(true, true, [tellMessage("tell", "verified_result", "x")], undefined, "coord"), { wrapper });
	expect(loading.result.current.settled).toBe(false);
	expect(loading.result.current.older.ids).toEqual([]);
});

// The hook runs in every orchestrator chat, but the notifications are global: a repo manager's chat has no
// coordinator blocks, so the coordinator's older messages must never become a pill there.
it("counts only notifications addressed to the session whose chat it is", () => {
	const forSession = (id: string, sessionId: string, purpose: NonNullable<NotificationDTO["purpose"]>, at: string): NotificationDTO => ({
		...note(`ntf_${id}`, purpose, "text"), messageId: id, sessionId, createdAt: at,
	});
	const notifications = [
		forSession("tell-1", "coord", "verified_result", "2026-10-26T09:00:00Z"),
		forSession("tell-2", "coord", "stopped_work", "2026-10-26T10:00:00Z"),
	];
	const openAsks = [forSession("decision-1", "coord", "blocking_decision", "2026-10-26T08:00:00Z")];
	const manager = mount({ items: [], notifications, openAsks, sessionId: "manager-1" });
	expect(manager.result.current.older.ids).toEqual([]);
	const coordinator = mount({ items: [], notifications, openAsks, sessionId: "coord" });
	expect(coordinator.result.current.older.ids).toEqual(["decision-1", "tell-1", "tell-2"]);
});

it("orders older messages deterministically even when a timestamp cannot be parsed", () => {
	const at = (id: string, createdAt: string): NotificationDTO => ({ ...note(`ntf_${id}`, "verified_result", "text"), messageId: id, createdAt });
	const { result } = mount({ items: [], notifications: [at("b", "not a date"), at("a", "not a date"), at("c", "2026-10-26T09:00:00Z")] });
	expect(result.current.older.ids).toEqual(["a", "b", "c"]);
});

it("does not ask for the open questions before the first notification page has loaded", () => {
	getMock.mockClear();
	const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
	const wrapper = ({ children }: { children: ReactNode }) => <QueryClientProvider client={client}>{children}</QueryClientProvider>;
	renderHook(() => useReadCoordinatorTells(true, true, [], undefined, "coord"), { wrapper });
	const askedForOpenQuestions = getMock.mock.calls.some((call) => JSON.stringify(call).includes('"status":"unresolved"'));
	expect(askedForOpenQuestions).toBe(false);
});
