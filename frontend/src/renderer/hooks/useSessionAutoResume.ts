import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { apiClient, apiErrorMessage, hasTrustedApiBaseUrl } from "../lib/api-client";

export type AutoResumePolicy = "allowed" | "never";

export const sessionAutoResumeQueryKey = (sessionId: string) => ["session-auto-resume", sessionId] as const;

/** A session's auto-resume policy. `enabled` lets a caller skip the request for sessions it cannot apply to. */
export function useSessionAutoResumePolicy(sessionId: string, enabled = true) {
	return useQuery({
		queryKey: sessionAutoResumeQueryKey(sessionId),
		enabled: enabled && hasTrustedApiBaseUrl(),
		// Every session tab menu asks; the policy changes rarely and the lift refreshes it itself.
		staleTime: 30_000,
		refetchOnWindowFocus: false,
		queryFn: async (): Promise<AutoResumePolicy> => {
			const { data, error } = await apiClient.GET("/api/v1/sessions/{sessionId}/auto-resume", {
				params: { path: { sessionId } },
			});
			if (error || !data) throw new Error(apiErrorMessage(error));
			return data.policy;
		},
	});
}

/**
 * Lifts a session's `never` protection. Only the app may do this: the request carries `confirmedInApp`, the
 * marker the daemon requires, so it is sent from the confirm dialog and nowhere else.
 */
export function useAllowAutoResume(sessionId: string) {
	const queryClient = useQueryClient();
	return useMutation({
		mutationFn: async () => {
			const { error, response } = await apiClient.PUT("/api/v1/sessions/{sessionId}/auto-resume", {
				params: { path: { sessionId } },
				body: { policy: "allowed", confirmedInApp: true },
			});
			if (error) throw new Error(apiErrorMessage(error, response ? `Request failed (${response.status})` : "Request failed"));
		},
		onSuccess: () => queryClient.invalidateQueries({ queryKey: sessionAutoResumeQueryKey(sessionId) }),
	});
}
