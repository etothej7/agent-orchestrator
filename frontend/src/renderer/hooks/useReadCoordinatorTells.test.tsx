import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import type { NotificationDTO } from "../lib/notifications";
import type { ConversationItem } from "../types/conversation";

const { markMock } = vi.hoisted(() => ({ markMock: vi.fn() }));
vi.mock("../lib/notifications", async (importOriginal) => ({
	...(await importOriginal<typeof import("../lib/notifications")>()),
	markAllNotificationsRead: markMock,
}));
vi.mock("../lib/api-client", async (importOriginal) => ({
	...(await importOriginal<typeof import("../lib/api-client")>()),
	apiClient: { GET: () => new Promise(() => undefined) },
}));

import { useReadCoordinatorTells } from "./useReadCoordinatorTells";

const tellMessage = (id: string, purpose: string, text: string): ConversationItem => ({
	kind: "message", id, sequence: 1, revision: 1, role: "assistant", origin: "daemon", streaming: false, text, purpose, createdAt: "2026-10-26T12:00:00Z",
});
const note = (id: string, purpose: NonNullable<NotificationDTO["purpose"]>, body: string, status: "read" | "unread" = "unread", messageId = id.replace("ntf_", "")): NotificationDTO => ({
	id, sessionId: "coord", projectId: "overall", prUrl: "", type: "coordinator_message", purpose, messageId, title: id, body, status,
	createdAt: "2026-10-26T12:00:01Z", target: { kind: "session", sessionId: "coord" },
});

function mount(options: { enabled?: boolean; onScreen?: boolean; items: ConversationItem[]; notifications: NotificationDTO[] }) {
	const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
	client.setQueryData(["notifications", "history", "all"], { pageParams: [""], pages: [{ notifications: options.notifications, unreadCount: 0, unresolvedCount: 0, unresolvedAskCount: 0 }] });
	const wrapper = ({ children }: { children: ReactNode }) => <QueryClientProvider client={client}>{children}</QueryClientProvider>;
	return renderHook(() => useReadCoordinatorTells(options.enabled ?? true, options.onScreen ?? true, options.items), { wrapper });
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
