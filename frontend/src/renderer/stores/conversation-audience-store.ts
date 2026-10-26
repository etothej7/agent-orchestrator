import { create } from "zustand";

/**
 * Which slice of a conversation a view reads. `human` is the daemon's default:
 * for the main coordinator it holds only what Ed started, steered or was told.
 * `all` is the unfiltered transcript, including automation turns and activity.
 */
export type ConversationAudience = "human" | "all";

interface ConversationAudienceState {
	/** Sessions whose view currently includes internal activity. Never persisted: it is off on every launch. */
	showInternal: Readonly<Record<string, true>>;
	setShowInternal: (sessionId: string, show: boolean) => void;
}

export const useConversationAudienceStore = create<ConversationAudienceState>((set) => ({
	showInternal: {},
	setShowInternal: (sessionId, show) =>
		set((state) => {
			if (Boolean(state.showInternal[sessionId]) === show) return state;
			const next = { ...state.showInternal };
			if (show) next[sessionId] = true;
			else delete next[sessionId];
			return { showInternal: next };
		}),
}));

export function conversationAudienceFor(sessionId: string): ConversationAudience {
	return useConversationAudienceStore.getState().showInternal[sessionId] ? "all" : "human";
}

export function useShowInternalActivity(sessionId: string): [boolean, (show: boolean) => void] {
	const show = useConversationAudienceStore((state) => Boolean(state.showInternal[sessionId]));
	const setShowInternal = useConversationAudienceStore((state) => state.setShowInternal);
	return [show, (next) => setShowInternal(sessionId, next)];
}
