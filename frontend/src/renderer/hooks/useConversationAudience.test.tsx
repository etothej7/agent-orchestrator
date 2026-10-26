import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";

const { getMock, postMock } = vi.hoisted(() => ({ getMock: vi.fn(), postMock: vi.fn() }));
vi.mock("../lib/api-client", async (importOriginal) => ({
	...(await importOriginal<typeof import("../lib/api-client")>()),
	apiClient: { GET: getMock, POST: postMock, PATCH: vi.fn() },
}));

import { useConversationAudienceStore } from "../stores/conversation-audience-store";
import { conversationQueryKey, toSnapshot, useConversation, useConversationCommands } from "./useConversation";

const message = (id: string, sequence: number, extra: Record<string, unknown> = {}) => ({
	id,
	turnId: `turn-${sequence}`,
	sequence,
	revision: 1,
	role: "assistant",
	origin: "provider",
	text: id,
	streaming: false,
	editAvailable: false,
	kind: "message",
	createdAt: "2026-10-25T10:00:00Z",
	...extra,
});
const wire = (messages: unknown[]) => ({
	conversationId: "conv-1",
	sessionId: "coord-1",
	harness: "codex",
	mode: "chat",
	controller: "ready",
	latestSequence: 9,
	oldestSequence: 1,
	hasMoreBefore: false,
	settings: {},
	turns: [],
	activities: [],
	messages,
});

let queryClient: QueryClient;
const wrapper = ({ children }: { children: ReactNode }) => <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
const audienceParam = (call: unknown[]) => (call[1] as { params: { query: { audience?: string } } }).params.query.audience;

beforeEach(() => {
	queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
	useConversationAudienceStore.setState({ showInternal: {} });
	getMock.mockReset();
});
afterEach(() => useConversationAudienceStore.setState({ showInternal: {} }));

it("keeps the unfiltered view under its own key beneath the session key", () => {
	expect(conversationQueryKey("coord-1")).toEqual(["conversation", "coord-1"]);
	expect(conversationQueryKey("coord-1", undefined, "all")).toEqual(["conversation", "coord-1", "all"]);
	useConversationAudienceStore.getState().setShowInternal("coord-1", true);
	expect(conversationQueryKey("coord-1")).toEqual(["conversation", "coord-1", "all"]);
	expect(conversationQueryKey("coord-2")).toEqual(["conversation", "coord-2"]);
	// A remote host has no unfiltered view, whatever the local toggle says.
	expect(conversationQueryKey("coord-1", "host-a")).toEqual(["remote-conversation", "host-a", "coord-1"]);
});

it("reads the default audience until internal activity is switched on, then asks for audience=all", async () => {
	getMock.mockImplementation(async (_path: string, init: { params: { query: { audience?: string } } }) => ({
		data: init.params.query.audience === "all"
			? wire([message("reply", 1, { audience: "human" }), message("routine", 2, { audience: "internal", origin: "automation" })])
			: wire([message("reply", 1, { audience: "human" })]),
	}));
	const { result } = renderHook(() => useConversation("coord-1"), { wrapper });
	await waitFor(() => expect(result.current.snapshot?.items.map((item) => item.id)).toEqual(["reply"]));
	expect(audienceParam(getMock.mock.calls[0]!)).toBeUndefined();

	act(() => useConversationAudienceStore.getState().setShowInternal("coord-1", true));
	await waitFor(() => expect(result.current.snapshot?.items.map((item) => item.id)).toEqual(["reply", "routine"]));
	expect(audienceParam(getMock.mock.calls.at(-1)!)).toBe("all");
	expect(result.current.snapshot?.items.map((item) => (item.kind === "message" ? item.audience : undefined))).toEqual(["human", "internal"]);

	// Switching back returns to the filtered rows; nothing from the full view leaks into it.
	act(() => useConversationAudienceStore.getState().setShowInternal("coord-1", false));
	await waitFor(() => expect(result.current.snapshot?.items.map((item) => item.id)).toEqual(["reply"]));
});

it("keeps the previous rows on screen while the other audience loads", async () => {
	let release!: () => void;
	const gate = new Promise<void>((resolve) => { release = resolve; });
	getMock.mockImplementation(async (_path: string, init: { params: { query: { audience?: string } } }) => {
		if (init.params.query.audience === "all") await gate;
		return { data: wire([message("reply", 1, { audience: "human" })]) };
	});
	const { result } = renderHook(() => useConversation("coord-1"), { wrapper });
	await waitFor(() => expect(result.current.snapshot).toBeDefined());
	act(() => useConversationAudienceStore.getState().setShowInternal("coord-1", true));
	expect(result.current.snapshot?.items.map((item) => item.id)).toEqual(["reply"]);
	expect(result.current.isLoading).toBe(false);
	release();
});

it("refreshes the open unfiltered view when a change event invalidates the session's conversation", async () => {
	let replies = [message("reply", 1, { audience: "human" })];
	getMock.mockImplementation(async () => ({ data: wire(replies) }));
	useConversationAudienceStore.getState().setShowInternal("coord-1", true);
	const { result } = renderHook(() => useConversation("coord-1"), { wrapper });
	await waitFor(() => expect(result.current.snapshot?.items).toHaveLength(1));

	// The change stream invalidates the session's default key; the prefix match must reach the "all" view.
	replies = [...replies, message("told-ed", 2, { audience: "human" })];
	await act(async () => {
		await queryClient.invalidateQueries({ queryKey: ["conversation", "coord-1"] });
	});
	await waitFor(() => expect(result.current.snapshot?.items.map((item) => item.id)).toEqual(["reply", "told-ed"]));
});

it("never shows internal rows in the default view while it loads after the toggle goes off", async () => {
	let releaseHuman!: () => void;
	const humanGate = new Promise<void>((resolve) => { releaseHuman = resolve; });
	getMock.mockImplementation(async (_path: string, init: { params: { query: { audience?: string } } }) => {
		if (init.params.query.audience === "all") {
			return { data: wire([message("reply", 1, { audience: "human" }), message("routine", 2, { audience: "internal", origin: "automation" })]) };
		}
		await humanGate;
		return { data: wire([message("reply", 1, { audience: "human" })]) };
	});
	useConversationAudienceStore.getState().setShowInternal("coord-1", true);
	const { result } = renderHook(() => useConversation("coord-1"), { wrapper });
	await waitFor(() => expect(result.current.snapshot?.items.map((item) => item.id)).toEqual(["reply", "routine"]));

	act(() => useConversationAudienceStore.getState().setShowInternal("coord-1", false));
	// The default view is still loading; the all-view rows are on screen only minus the internal ones.
	expect(result.current.snapshot?.items.map((item) => item.id)).toEqual(["reply"]);
	expect(result.current.isLoading).toBe(false);
	releaseHuman();
	await waitFor(() => expect(getMock.mock.calls.some((call) => audienceParam(call) === undefined)).toBe(true));
});

it("rolls a failed queue reorder back into the cache it edited, even if the audience flipped in flight", async () => {
	const queued = (audience: "human" | "all", latestSequence: number) => ({
		pages: [toSnapshot({ ...wire([]), latestSequence, turns: [
			{ id: "q1", state: "queued", requestedAt: "2026-10-25T10:00:00Z" },
			{ id: "q2", state: "queued", requestedAt: "2026-10-25T10:01:00Z" },
		] } as never)],
		pageParams: [undefined],
		audience,
	});
	const human = queued("human", 1);
	const all = queued("all", 99);
	queryClient.setQueryData(conversationQueryKey("coord-1", undefined, "human"), { pages: human.pages, pageParams: human.pageParams });
	queryClient.setQueryData(conversationQueryKey("coord-1", undefined, "all"), { pages: all.pages, pageParams: all.pageParams });
	let fail!: () => void;
	postMock.mockImplementation(() => new Promise((resolve) => { fail = () => resolve({ error: { message: "nope" }, data: undefined }); }));

	const { result } = renderHook(() => useConversationCommands("coord-1"), { wrapper });
	let outcome!: Promise<unknown>;
	act(() => { outcome = result.current.reorderQueuedTurns(["q2", "q1"]).catch((error: unknown) => error); });
	await waitFor(() => expect(postMock).toHaveBeenCalled());
	// Flip the view while the request is in flight, then let it fail.
	act(() => useConversationAudienceStore.getState().setShowInternal("coord-1", true));
	await act(async () => { fail(); await outcome; });

	const humanPages = queryClient.getQueryData<{ pages: { latestSequence: number }[] }>(["conversation", "coord-1"]);
	const allPages = queryClient.getQueryData<{ pages: { latestSequence: number }[] }>(["conversation", "coord-1", "all"]);
	expect(humanPages?.pages[0].latestSequence).toBe(1);
	expect(allPages?.pages[0].latestSequence).toBe(99);
});
