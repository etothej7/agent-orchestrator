import { useCallback, useState } from "react";
import { apiErrorMessage } from "../lib/api-client";
import { useNavigateToSession } from "../lib/navigate-to-session";
import { useCoordinatorJumpStore } from "../stores/coordinator-jump-store";
import { spawnOrchestrator } from "../lib/spawn-orchestrator";
import { newestActiveOrchestrator, type WorkspaceSummary } from "../types/workspace";
import { useCoordinationQuery } from "./useCoordinationQuery";

export type OpenMainCoordinatorOutcome = "opened" | "none" | "failed";

/**
 * Opens the persistent main coordinator conversation: the newest active
 * orchestrator of the coordinator project. It never starts one unless the
 * caller says `spawn: true` (Home → Open main, once the workspace has loaded);
 * a list that has not loaded yet looks the same as "no coordinator" and adding
 * or replacing a coordinator is not something a navigation row may do.
 */
export function useOpenMainCoordinator(workspaces: readonly Pick<WorkspaceSummary, "id" | "sessions">[] | undefined) {
	const config = useCoordinationQuery();
	const navigate = useNavigateToSession();
	const [busy, setBusy] = useState(false);
	const [error, setError] = useState("");
	const projectId = config.data?.coordinatorProjectId || undefined;
	const open = useCallback(async ({ spawn = false }: { spawn?: boolean } = {}): Promise<OpenMainCoordinatorOutcome> => {
		if (!projectId || busy) return "failed";
		setBusy(true);
		setError("");
		try {
			const row = workspaces?.find((project) => project.id === projectId);
			const existing = row && newestActiveOrchestrator(row.sessions);
			if (existing) {
				useCoordinatorJumpStore.getState().request();
				navigate(projectId, existing.id);
				return "opened";
			}
			if (!spawn) return "none";
			navigate(projectId, await spawnOrchestrator(projectId, "sidebar", false, "chat"));
			return "opened";
		} catch (err) {
			setError(apiErrorMessage(err));
			return "failed";
		} finally {
			setBusy(false);
		}
	}, [busy, navigate, projectId, workspaces]);
	return {
		/** The coordinator project, or undefined until coordination is configured. */
		projectId,
		configLoaded: config.isSuccess,
		configError: config.isError,
		busy,
		error,
		open,
	};
}
