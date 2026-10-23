import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { apiClient } from "../../lib/api-client";
import { spawnOrchestrator } from "../../lib/spawn-orchestrator";
import { CoordinationSettingsSection } from "./CoordinationSettingsSection";

vi.mock("@tanstack/react-router", () => ({ useNavigate: () => vi.fn() }));
vi.mock("../../lib/spawn-orchestrator", () => ({ spawnOrchestrator: vi.fn(async () => "coordinator-session") }));
vi.mock("../../hooks/useSettings", () => ({ useSettings: () => ({ settings: { chatHarnesses: ["codex"] } }) }));

const projects = [
	{ id: "overall", name: "Overall coordinator", kind: "scratch", orchestratorAgent: "codex" },
	{ id: "api", name: "API repository", kind: "repository", orchestratorAgent: "codex" },
];

beforeEach(() => {
	vi.spyOn(apiClient, "GET").mockImplementation(async (path) => {
		if (path === "/api/v1/projects") return { data: { projects } } as never;
		if (path === "/api/v1/settings/coordination") return { data: { coordination: { coordinatorProjectId: "overall", managedProjectIds: [] } } } as never;
		throw new Error(`Unexpected GET ${path}`);
	});
});
afterEach(() => { vi.restoreAllMocks(); vi.clearAllMocks(); });

function renderSettings() {
	const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
	return render(<QueryClientProvider client={client}><CoordinationSettingsSection /></QueryClientProvider>);
}

describe("Coordinator setup", () => {
	it("offers a dedicated coordinator even when an unrelated Scratch project exists", async () => {
		renderSettings();
		await screen.findByRole("checkbox", { name: "API repository" });
		expect(screen.getByRole("button", { name: "Create coordinator" })).toBeEnabled();
	});

	it("saves selected repos without starting agents, then opens only on request", async () => {
		const put = vi.spyOn(apiClient, "PUT").mockResolvedValue({ data: { coordination: { coordinatorProjectId: "overall", managedProjectIds: ["api"] } } } as never);
		renderSettings();
		await userEvent.click(await screen.findByRole("checkbox", { name: "API repository" }));
		expect(screen.getByRole("button", { name: "Open coordinator" })).toBeDisabled();
		await userEvent.click(screen.getByRole("button", { name: "Save assignments" }));
		expect(await screen.findByRole("status")).toHaveTextContent("Assignments saved");
		expect(put).toHaveBeenCalledWith("/api/v1/settings/coordination", { body: { coordinatorProjectId: "overall", managedProjectIds: ["api"] } });
		expect(spawnOrchestrator).not.toHaveBeenCalled();
		await userEvent.click(screen.getByRole("button", { name: "Open coordinator" }));
		await waitFor(() => expect(spawnOrchestrator).toHaveBeenCalledWith("overall", "settings", false, "chat"));
	});

	it("keeps the draft and prevents opening it when a live manager blocks reassignment", async () => {
		vi.spyOn(apiClient, "PUT").mockResolvedValue({ error: { message: "Finish and stop the affected orchestrator before changing its assignment" } } as never);
		renderSettings();
		await userEvent.click(await screen.findByRole("checkbox", { name: "API repository" }));
		await userEvent.click(screen.getByRole("button", { name: "Save assignments" }));
		expect(await screen.findByRole("alert")).toHaveTextContent("Finish and stop the affected orchestrator");
		expect(screen.getByRole("checkbox", { name: "API repository" })).toBeChecked();
		expect(screen.getByRole("button", { name: "Save assignments" })).toBeEnabled();
		expect(screen.getByRole("button", { name: "Open coordinator" })).toBeDisabled();
		expect(spawnOrchestrator).not.toHaveBeenCalled();
	});
});
