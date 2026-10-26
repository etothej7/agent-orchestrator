import type { ConversationItem, ConversationMessage } from "../types/conversation";
import type { NotificationDTO } from "./notifications";

/** What a coordinator message tells Ed: a result to note, work that stopped, or a question that needs him. */
export type CoordinatorMessageKind = "result" | "stopped" | "ask";

const KIND_BY_PURPOSE: Readonly<Record<string, CoordinatorMessageKind>> = {
	verified_result: "result",
	stopped_work: "stopped",
	scope_question: "ask",
	blocking_decision: "ask",
	completion_decision: "ask",
};

export function coordinatorKindOfPurpose(purpose: string | undefined): CoordinatorMessageKind | undefined {
	return purpose ? KIND_BY_PURPOSE[purpose] : undefined;
}

/**
 * A message the coordinator deliberately addressed to Ed (`ao tell` / `ao ask`). The daemon
 * stores it with no turn, so it is recognised by its purpose, never by where it sits.
 */
export function coordinatorMessageKind(item: ConversationItem | undefined): CoordinatorMessageKind | undefined {
	return item?.kind === "message" && item.role === "assistant" ? coordinatorKindOfPurpose(item.purpose) : undefined;
}

export function isCoordinatorMessage(item: ConversationItem | undefined): boolean {
	return coordinatorMessageKind(item) !== undefined;
}

/**
 * The notification the daemon raised for a coordinator message: a coordinator_message with the same
 * purpose whose body opens the message text. The notification carries no message id, so this pairs
 * them by what they say; the one created closest to the message wins.
 */
export function notificationForMessage(
	message: Pick<ConversationMessage, "text" | "purpose" | "createdAt">,
	notifications: readonly NotificationDTO[],
): NotificationDTO | undefined {
	const text = message.text.trim();
	const sent = Date.parse(message.createdAt);
	return notifications
		.filter((notification) => {
			if (notification.type !== "coordinator_message" || notification.purpose !== message.purpose) return false;
			const body = notification.body.replace(/…$/, "").trim();
			return body.length > 0 && text.startsWith(body);
		})
		.sort((a, b) => Math.abs(Date.parse(a.createdAt) - sent) - Math.abs(Date.parse(b.createdAt) - sent))[0];
}
