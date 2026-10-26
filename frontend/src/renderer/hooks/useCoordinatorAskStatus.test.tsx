import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import type { NotificationDTO } from "../lib/notifications";
import type { ConversationMessage } from "../types/conversation";

const { getMock } = vi.hoisted(() => ({ getMock: vi.fn() }));
vi.mock("../lib/api-client", async (importOriginal) => ({
	...(await importOriginal<typeof import("../lib/api-client")>()),
	apiClient: { GET: getMock },
}));

import { useCoordinatorAskStatus } from "./useCoordinatorAskStatus";

const NOW = Date.parse("2026-10-26T12:00:00Z");
const message = (over: Partial<ConversationMessage> = {}): ConversationMessage => ({
	kind: "message", id: "decision-q1", sequence: 5, revision: 1, role: "assistant", origin: "daemon", streaming: false,
	text: "Which migration order should we use?", purpose: "blocking_decision", createdAt: "2026-10-26T09:00:00Z", ...over,
});
const notification = (over: Partial<NotificationDTO> = {}): NotificationDTO => ({
	id: "ntf_1", sessionId: "coord", projectId: "overall", prUrl: "", type: "coordinator_message", purpose: "blocking_decision", messageId: "decision-q1",
	title: "Your coordinator needs a decision", body: "Which migration order should we use?", status: "read", createdAt: "2026-10-26T09:00:01Z",
	target: { kind: "session", sessionId: "coord" }, ...over,
});

function mount(history: NotificationDTO[], openList: NotificationDTO[] | "pending" | "error", openCount = 1) {
	getMock.mockImplementation(() => openList === "pending" ? new Promise(() => undefined) : openList === "error" ? Promise.resolve({ error: { message: "x" } }) : Promise.resolve({ data: { notifications: openList } }));
	const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
	client.setQueryData(["notifications", "history", "all"], { pageParams: [""], pages: [{ notifications: history, unreadCount: 0, unresolvedCount: openCount, unresolvedAskCount: openCount }] });
	const wrapper = ({ children }: { children: ReactNode }) => <QueryClientProvider client={client}>{children}</QueryClientProvider>;
	return renderHook(() => useCoordinatorAskStatus(true), { wrapper });
}

beforeEach(() => { vi.useFakeTimers({ toFake: ["Date"], now: NOW }); getMock.mockReset(); });
afterEach(() => vi.useRealTimers());

it("is keyed on the message id the notification carries: open while unresolved", () => {
	expect(mount([notification()], "pending").result.current(message())).toEqual({ open: true });
});

it("reads as answered, or resolved by the coordinator, once the daemon resolved it", () => {
	expect(mount([notification({ resolvedAt: "2026-10-26T10:00:00Z", resolution: "answered" })], "pending").result.current(message())).toEqual({ open: false, resolution: "answered" });
	expect(mount([notification({ resolvedAt: "2026-10-26T10:00:00Z", resolution: "coordinator" })], "pending").result.current(message())).toEqual({ open: false, resolution: "coordinator" });
});

it("never pairs by text: a different message with identical words is not this ask", () => {
	const other = notification({ id: "ntf_2", messageId: "decision-other", resolvedAt: "2026-10-26T10:00:00Z", resolution: "answered" });
	expect(mount([other, notification()], "pending").result.current(message())).toEqual({ open: true });
});

it("an old open ask that scrolled out of the loaded history is found in the daemon's open list and stays open", async () => {
	const { result } = mount([], [notification()]);
	await waitFor(() => expect(result.current(message())).toEqual({ open: true }));
});

it("an old ask absent from a loaded open list is settled", async () => {
	const { result } = mount([], []);
	await waitFor(() => expect(result.current(message())).toEqual({ open: false, resolution: "answered" }));
});

it("unknown reads as OPEN: no open list yet, or it failed to load", async () => {
	expect(mount([], "pending").result.current(message())).toBeUndefined();
	const failed = mount([], "error");
	await waitFor(() => expect(getMock).toHaveBeenCalled());
	expect(failed.result.current(message())).toBeUndefined();
});

it("a question that only just arrived is not called settled before its notification exists", async () => {
	const { result } = mount([], []);
	await waitFor(() => expect(getMock).toHaveBeenCalled());
	await waitFor(() => expect(result.current(message({ createdAt: "2026-10-26T11:59:30Z" }))).toBeUndefined());
});

it("says nothing about messages that are not questions", () => {
	expect(mount([notification()], "pending").result.current(message({ purpose: "verified_result" }))).toBeUndefined();
});
