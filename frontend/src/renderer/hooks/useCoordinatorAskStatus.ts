import { useCallback, useMemo } from "react";
import type { CoordinatorAskStatus } from "../components/chat/coordinator-asks";
import { coordinatorKindOfPurpose, notificationForMessage } from "../lib/coordinator-messages";
import type { ConversationMessage } from "../types/conversation";
import { useNotificationsQuery } from "./useNotificationsQuery";

const RECENT_MS = 2 * 60_000;

/**
 * Whether each coordinator question in the chat is still open, read from the notification history the
 * live stream keeps current (resolution is explicit: Answer, `ao ask resolve`, or dismiss).
 * A question with no notification left was dismissed or cleared, so it reads as settled.
 */
export function useCoordinatorAskStatus(enabled: boolean): (message: ConversationMessage) => CoordinatorAskStatus {
	const history = useNotificationsQuery("all", enabled);
	const asks = useMemo(
		() =>
			(history.data?.pages.flatMap((page) => page.notifications) ?? []).filter(
				(notification) => notification.type === "coordinator_message" && coordinatorKindOfPurpose(notification.purpose) === "ask",
			),
		[history.data],
	);
	return useCallback(
		(message) => {
			if (coordinatorKindOfPurpose(message.purpose) !== "ask") return undefined;
			const notification = notificationForMessage(message, asks);
			if (notification) {
				return notification.resolvedAt
					? { open: false, resolution: notification.resolution ?? "answered" }
					: { open: true };
			}
			// The notification may still be on its way for a question that just arrived.
			const age = Date.now() - Date.parse(message.createdAt);
			return age < RECENT_MS ? undefined : { open: false, resolution: "answered" };
		},
		[asks],
	);
}
