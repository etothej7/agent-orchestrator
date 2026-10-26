import { useQuery } from "@tanstack/react-query";
import { apiClient, apiErrorMessage, hasTrustedApiBaseUrl } from "../lib/api-client";

/** `enabled` lets a caller that can only matter for some sessions (a local orchestrator) skip the request. */
export function useCoordinationQuery(enabled = true) {
	return useQuery({ queryKey: ["coordination"], enabled: enabled && hasTrustedApiBaseUrl(), queryFn: async () => {
		const { data, error } = await apiClient.GET("/api/v1/settings/coordination");
		if (error || !data) throw new Error(apiErrorMessage(error));
		return data.coordination;
	} });
}
