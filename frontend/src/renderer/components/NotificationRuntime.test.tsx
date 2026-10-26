import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import type { NotificationDTO, NotificationsCache } from "../lib/notifications";
import { NotificationRuntime } from "./NotificationCenter";

const notifications: NotificationDTO[] = [
	{
		id: "ntf_1",
		sessionId: "sess-1",
		projectId: "proj-1",
		prUrl: "",
		type: "needs_input",
		title: "Needs input",
		body: "",
		status: "unread",
		createdAt: "2026-06-16T10:00:00Z",
		target: { kind: "session", sessionId: "sess-1" },
	},
	{
		id: "ntf_2",
		sessionId: "sess-2",
		projectId: "proj-1",
		prUrl: "",
		type: "ready_to_merge",
		title: "Ready to merge",
		body: "",
		status: "unread",
		createdAt: "2026-06-16T11:00:00Z",
		target: { kind: "session", sessionId: "sess-2" },
	},
];

const coordinator = (id: string, purpose: NonNullable<NotificationDTO["purpose"]>, status: "read" | "unread"): NotificationDTO => ({
	id, sessionId: "coord", projectId: "overall", prUrl: "", type: "coordinator_message", purpose, title: id, body: id, status,
	createdAt: "2026-06-16T12:00:00Z", target: { kind: "session", sessionId: "coord" },
});

const cacheWith = (extra: NotificationDTO[], unresolvedAskCount: number): NotificationsCache => ({
	pages: [{ notifications: [...notifications, ...extra], nextCursor: undefined, unreadCount: notifications.length + extra.length, unresolvedCount: notifications.length + unresolvedAskCount, unresolvedAskCount }],
	pageParams: [""],
});
let unreadCache: NotificationsCache = cacheWith([], 0);

const { setBadge } = vi.hoisted(() => ({ setBadge: vi.fn() }));

vi.mock("@tanstack/react-router", () => ({ useNavigate: () => vi.fn(), useParams: () => ({}) }));

vi.mock("../hooks/useNotificationsQuery", () => ({
	useNotificationsQuery: () => ({ data: unreadCache, isError: false }),
}));

vi.mock("../lib/notifications", async (importOriginal) => ({
	...((await importOriginal()) as object),
	createNotificationsTransport: () => ({ connect: () => undefined }),
}));

vi.mock("../lib/bridge", () => ({
	aoBridge: {
		notifications: {
			setBadge,
			show: vi.fn(),
			devBounce: vi.fn(),
			onClick: () => () => undefined,
		},
	},
}));

function renderRuntimeOnly() {
	const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
	return render(
		<QueryClientProvider client={queryClient}>
			{/* NotificationCenter (the bell) is intentionally NOT mounted here. */}
			<NotificationRuntime />
		</QueryClientProvider>,
	);
}

describe("NotificationRuntime badge sync", () => {
	it("syncs the launcher badge without the notification bell being mounted", async () => {
		setBadge.mockClear();
		// Two open questions and one unread result; a read stop and ordinary unread agent notifications do not count.
		unreadCache = cacheWith([
			coordinator("tell-unread", "verified_result", "unread"),
			coordinator("stop-read", "stopped_work", "read"),
			coordinator("ask-open-1", "blocking_decision", "unread"),
			coordinator("ask-open-2", "scope_question", "read"),
		], 2);

		renderRuntimeOnly();

		// The badge must update purely from the always-mounted runtime, independent
		// of NotificationCenter, which Linux hides from the topbar and only mounts
		// on the sessions board.
		await waitFor(() => expect(setBadge).toHaveBeenCalledWith(3));
		expect(setBadge).not.toHaveBeenCalledWith(notifications.length);
	});

	it("is clear when nothing waits on Ed, whatever else is unread", async () => {
		setBadge.mockClear();
		unreadCache = cacheWith([coordinator("stop-read", "stopped_work", "read")], 0);

		renderRuntimeOnly();

		await waitFor(() => expect(setBadge).toHaveBeenCalledWith(0));
	});

	it("takes the open-question count from the daemon, not from the loaded page", async () => {
		setBadge.mockClear();
		unreadCache = cacheWith([], 4);

		renderRuntimeOnly();

		await waitFor(() => expect(setBadge).toHaveBeenCalledWith(4));
	});
});
