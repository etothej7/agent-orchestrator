import type { ConversationItem } from "../types/conversation";

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
