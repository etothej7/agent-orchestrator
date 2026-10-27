import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { components } from "../../api/schema";
import { apiClient, apiErrorMessage, hasTrustedApiBaseUrl } from "../lib/api-client";
import { sessionAutoResumeQueryKey } from "./useSessionAutoResume";

export type HumanRequestDTO = components["schemas"]["ControllersHumanRequestResponse"];

export const humanRequestsQueryKey = ["human-requests", "pending"] as const;

/** How often the open requests are re-read. A request is rare and a click is the point, so seconds are fine. */
const POLL_MS = 10_000;

/** The requests waiting for the person, newest first. Stays empty (and silent) until the daemon is trusted. */
export function useHumanRequestsQuery() {
	return useQuery({
		queryKey: humanRequestsQueryKey,
		enabled: hasTrustedApiBaseUrl(),
		refetchInterval: POLL_MS,
		refetchIntervalInBackground: false,
		queryFn: async (): Promise<HumanRequestDTO[]> => {
			const { data, error } = await apiClient.GET("/api/v1/human-requests", { params: { query: { state: "pending" } } });
			if (error || !data) throw new Error(apiErrorMessage(error));
			return data.requests;
		},
	});
}

export type HumanRequestDecision = "approve" | "deny";

/**
 * Approves or denies a request as the person. An approval names the hash of the request as it was shown, so
 * the daemon refuses it if the card on screen is not the request it holds. The capability the daemon needs is
 * added to this call by the app's main process, never here.
 */
export function useDecideHumanRequest() {
	const queryClient = useQueryClient();
	return useMutation({
		mutationFn: async ({ request, decision }: { request: HumanRequestDTO; decision: HumanRequestDecision }) => {
			const params = { path: { id: request.id } };
			const { data, error, response } =
				decision === "approve"
					? await apiClient.POST("/api/v1/human-requests/{id}/approve", { params, body: { payloadHash: request.payloadHash } })
					: await apiClient.POST("/api/v1/human-requests/{id}/deny", { params });
			if (error || !data) throw new Error(apiErrorMessage(error, response ? `Request failed (${response.status})` : "Request failed"));
			return data.request;
		},
		onSettled: async (_data, _error, { request }) => {
			await Promise.all([
				queryClient.invalidateQueries({ queryKey: humanRequestsQueryKey }),
				// What an approval may have changed.
				queryClient.invalidateQueries({ queryKey: sessionAutoResumeQueryKey(request.sessionId) }),
				queryClient.invalidateQueries({ queryKey: ["workspace"] }),
			]);
		},
	});
}
