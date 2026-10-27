import { useQuery } from "@tanstack/react-query";
import { useCallback, useMemo } from "react";
import type { CoordinatorAskStatus } from "../components/chat/coordinator-asks";
import { apiClient } from "../lib/api-client";
import { coordinatorKindOfPurpose, notificationForMessage } from "../lib/coordinator-messages";
import type { NotificationDTO } from "../lib/notifications";
import type { ConversationMessage } from "../types/conversation";
import { useNotificationsQuery } from "./useNotificationsQuery";

const RECENT_MS = 2 * 60_000;

/**
 * Every coordinator question still waiting on Ed, straight from the daemon. The open list is small and
 * complete, so it answers "is this old question still open?" even when it has scrolled out of the
 * loaded notification history. Refetched whenever the daemon's open-question count changes.
 */
export function useOpenAsks(enabled: boolean, openCount: number | undefined) {
	return useQuery({
		queryKey: ["coordinator-open-asks", openCount ?? "unknown"],
		enabled,
		staleTime: 30_000,
		placeholderData: (previous) => previous,
		queryFn: async () => {
			const { data, error } = await apiClient.GET("/api/v1/notifications", { params: { query: { status: "unresolved", limit: 100 } } });
			if (error || !data) throw new Error("Could not load open questions");
			return data.notifications.filter((notification) => notification.type === "coordinator_message" && coordinatorKindOfPurpose(notification.purpose) === "ask");
		},
	});
}

/**
 * Whether each coordinator question in the chat is still open (resolution is explicit: Answer,
 * `ao ask resolve`, or dismiss). Keyed on the message id the notification carries. A question whose
 * state is not known is treated as OPEN: the Answer button never disappears while the daemon still
 * counts the question, and an answer to a settled one is validated and idempotent.
 */
export function useCoordinatorAskStatus(enabled: boolean): (message: ConversationMessage) => CoordinatorAskStatus {
	const history = useNotificationsQuery("all", enabled);
	const known = useMemo(
		() => (history.data?.pages.flatMap((page) => page.notifications) ?? []).filter((notification) => coordinatorKindOfPurpose(notification.purpose) === "ask"),
		[history.data],
	);
	const open = useOpenAsks(enabled, history.data?.pages[0]?.unresolvedAskCount);
	const openAsks: readonly NotificationDTO[] | undefined = open.data;
	return useCallback(
		(message) => {
			if (coordinatorKindOfPurpose(message.purpose) !== "ask") return undefined;
			const notification = notificationForMessage(message, known);
			if (notification) {
				return notification.resolvedAt
					? { open: false, resolution: notification.resolution ?? "answered" }
					: { open: true };
			}
			// Not in the loaded history: ask the daemon's complete open list. In it = open; absent from a list we
			// did load = settled (answered, resolved or dismissed); no list yet = unknown, which reads as open.
			if (openAsks === undefined) return undefined;
			if (notificationForMessage(message, openAsks)) return { open: true };
			// A question that has only just arrived may be ahead of its notification: not settled yet.
			return Date.now() - Date.parse(message.createdAt) < RECENT_MS ? undefined : { open: false, resolution: "answered" };
		},
		[known, openAsks],
	);
}
