import { useNotificationsQuery } from "./useNotificationsQuery";

/**
 * Whether the coordinator has asked Ed something he has not answered. The daemon
 * counts those (`unresolvedAskCount`) and the live stream adjusts the count as asks
 * are created and resolved, so the marker outlives the panel being opened (which
 * only marks notifications read).
 */
export function useCoordinatorAskPending(coordinatorProjectId: string | undefined): boolean {
	const history = useNotificationsQuery("all", Boolean(coordinatorProjectId));
	return Boolean(coordinatorProjectId && (history.data?.pages[0]?.unresolvedAskCount ?? 0) > 0);
}
