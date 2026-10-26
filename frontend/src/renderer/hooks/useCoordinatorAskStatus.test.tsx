import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderHook } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import type { NotificationDTO } from "../lib/notifications";
import type { ConversationMessage } from "../types/conversation";
import { useCoordinatorAskStatus } from "./useCoordinatorAskStatus";

vi.mock("../lib/api-client", async (importOriginal) => ({
	...(await importOriginal<typeof import("../lib/api-client")>()),
	apiClient: { GET: () => new Promise(() => undefined) },
}));

const NOW = Date.parse("2026-10-26T12:00:00Z");
const question = "Which migration order should we use?";
const message = (over: Partial<ConversationMessage> = {}): ConversationMessage => ({
	kind: "message", id: "decision-q1", sequence: 5, revision: 1, role: "assistant", origin: "daemon", streaming: false,
	text: `${question}\n\nWhy your input is needed: both touch billing.`, purpose: "blocking_decision", createdAt: "2026-10-26T09:00:00Z", ...over,
});
const notification = (over: Partial<NotificationDTO> = {}): NotificationDTO => ({
	id: "ntf_1", sessionId: "coord", projectId: "overall", prUrl: "", type: "coordinator_message", purpose: "blocking_decision",
	title: "Your coordinator needs a decision", body: question, status: "read", createdAt: "2026-10-26T09:00:01Z",
	target: { kind: "session", sessionId: "coord" }, ...over,
});

function statusOf(notifications: NotificationDTO[]) {
	const client = new QueryClient();
	client.setQueryData(["notifications", "history", "all"], { pageParams: [""], pages: [{ notifications, unreadCount: 0, unresolvedCount: 0, unresolvedAskCount: 0 }] });
	const wrapper = ({ children }: { children: ReactNode }) => <QueryClientProvider client={client}>{children}</QueryClientProvider>;
	return renderHook(() => useCoordinatorAskStatus(true), { wrapper }).result.current;
}

beforeEach(() => vi.useFakeTimers({ toFake: ["Date"], now: NOW }));
afterEach(() => vi.useRealTimers());

it("is open while its notification is unresolved", () => {
	expect(statusOf([notification()])(message())).toEqual({ open: true });
});

it("reads as answered, or resolved by the coordinator, once the daemon resolved it", () => {
	expect(statusOf([notification({ resolvedAt: "2026-10-26T10:00:00Z", resolution: "answered" })])(message())).toEqual({ open: false, resolution: "answered" });
	expect(statusOf([notification({ resolvedAt: "2026-10-26T10:00:00Z", resolution: "coordinator" })])(message())).toEqual({ open: false, resolution: "coordinator" });
});

it("pairs a question with its own notification, not another ask's", () => {
	const other = notification({ id: "ntf_2", body: "Something else entirely?", resolvedAt: "2026-10-26T10:00:00Z", resolution: "answered" });
	expect(statusOf([other, notification()])(message())).toEqual({ open: true });
});

it("does not match a different purpose with the same words", () => {
	expect(statusOf([notification({ purpose: "scope_question", resolvedAt: "2026-10-26T10:00:00Z" })])(message({ createdAt: "2026-10-26T09:00:00Z" }))).toEqual({ open: false, resolution: "answered" });
});

it("treats an old question whose notification is gone (dismissed or cleared) as settled", () => {
	expect(statusOf([])(message())).toEqual({ open: false, resolution: "answered" });
});

it("waits for the notification of a question that just arrived instead of calling it settled", () => {
	expect(statusOf([])(message({ createdAt: "2026-10-26T11:59:30Z" }))).toBeUndefined();
});

it("says nothing about messages that are not questions", () => {
	expect(statusOf([notification()])(message({ purpose: "verified_result" }))).toBeUndefined();
});
