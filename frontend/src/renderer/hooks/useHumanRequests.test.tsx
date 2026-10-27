import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { apiClient, setApiBaseUrl } from "../lib/api-client";
import { useHumanRequestsQuery } from "./useHumanRequests";

function wrapper() {
	const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
	return ({ children }: { children: ReactNode }) => <QueryClientProvider client={client}>{children}</QueryClientProvider>;
}

afterEach(() => {
	vi.restoreAllMocks();
	setApiBaseUrl(null);
});

describe("useHumanRequestsQuery", () => {
	it("stays silent until a daemon address is trusted", async () => {
		const get = vi.spyOn(apiClient, "GET");
		const { result } = renderHook(() => useHumanRequestsQuery(), { wrapper: wrapper() });
		await new Promise((resolve) => setTimeout(resolve, 20));
		expect(get).not.toHaveBeenCalled();
		expect(result.current.data).toBeUndefined();
	});

	it("reads the pending requests once the daemon is trusted", async () => {
		setApiBaseUrl("http://127.0.0.1:9");
		const get = vi.spyOn(apiClient, "GET").mockResolvedValue({ data: { requests: [{ id: "hr-1" }] }, response: { status: 200 } } as never);
		const { result } = renderHook(() => useHumanRequestsQuery(), { wrapper: wrapper() });
		await waitFor(() => expect(result.current.data).toEqual([{ id: "hr-1" }]));
		expect(get).toHaveBeenCalledWith("/api/v1/human-requests", { params: { query: { state: "pending" } } });
	});

	it("reports a failure instead of pretending nothing is waiting", async () => {
		setApiBaseUrl("http://127.0.0.1:9");
		vi.spyOn(apiClient, "GET").mockResolvedValue({ error: { message: "boom" }, response: { status: 500 } } as never);
		const { result } = renderHook(() => useHumanRequestsQuery(), { wrapper: wrapper() });
		await waitFor(() => expect(result.current.isError).toBe(true));
		expect(result.current.data).toBeUndefined();
	});
});
