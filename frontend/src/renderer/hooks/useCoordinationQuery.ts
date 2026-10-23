import { useQuery } from "@tanstack/react-query";
import { apiClient, apiErrorMessage, hasTrustedApiBaseUrl } from "../lib/api-client";

export function useCoordinationQuery() {
	return useQuery({ queryKey: ["coordination"], enabled: hasTrustedApiBaseUrl(), queryFn: async () => {
		const { data, error } = await apiClient.GET("/api/v1/settings/coordination");
		if (error || !data) throw new Error(apiErrorMessage(error));
		return data.coordination;
	} });
}
