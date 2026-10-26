import { create } from "zustand";

/** Opening the main coordinator from the sidebar, Home or a notification asks its chat to land on what waits for Ed. */
export const COORDINATOR_JUMP_REQUEST_MS = 30000;

interface CoordinatorJumpState {
	requestedAt?: number;
	request: () => void;
	clear: () => void;
}

export const useCoordinatorJumpStore = create<CoordinatorJumpState>((set) => ({
	requestedAt: undefined,
	request: () => set({ requestedAt: Date.now() }),
	clear: () => set({ requestedAt: undefined }),
}));

export const coordinatorJumpRequested = (requestedAt: number | undefined, now = Date.now()): boolean =>
	requestedAt !== undefined && now - requestedAt < COORDINATOR_JUMP_REQUEST_MS;
