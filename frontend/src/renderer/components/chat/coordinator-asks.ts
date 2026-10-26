import { createContext } from "react";
import type { ConversationMessage } from "../../types/conversation";

/** Whether a coordinator question is still waiting for Ed. Undefined: nothing is known about it. */
export type CoordinatorAskStatus = { open: boolean; resolution?: "answered" | "coordinator" } | undefined;

export interface CoordinatorAskControls {
	statusOf?(message: ConversationMessage): CoordinatorAskStatus;
	/** The question the composer is currently answering. */
	answeringId?: string;
	answer(message: ConversationMessage): void;
	/** A coordinator block entered or left the viewport; it counts as seen once it has been in view. */
	reportVisible?(messageId: string, visible: boolean): void;
}

export const CoordinatorAskContext = createContext<CoordinatorAskControls | undefined>(undefined);
