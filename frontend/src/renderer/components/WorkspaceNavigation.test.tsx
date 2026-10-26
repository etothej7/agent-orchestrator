import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, expect, it, vi } from "vitest";
import { TeamView } from "./TeamView";
import { WorkspaceEntry } from "./WorkspaceEntry";
import { useUiStore } from "../stores/ui-store";
const mocks = vi.hoisted(() => ({ open: vi.fn(), spawn: vi.fn(), coordination: { coordinatorProjectId: "overall" }, needsYou: false, rows: [] as unknown[], remote: [] as unknown[] }));
vi.mock("../hooks/useCoordinatorAskPending", () => ({ useCoordinatorAskPending: () => mocks.needsYou }));
vi.mock("../hooks/useCoordinationQuery", () => ({ useCoordinationQuery: () => ({ data: mocks.coordination, isSuccess: true }) }));
vi.mock("../hooks/useWorkspaceQuery", () => ({ useWorkspaceQuery: () => ({ data: mocks.rows, isSuccess: true }), useRemoteWorkspaces: () => ({ data: mocks.remote, failedHostIds: [] }) }));
vi.mock("../lib/navigate-to-session", () => ({ useNavigateToSession: () => mocks.open }));
vi.mock("../lib/spawn-orchestrator", () => ({ spawnOrchestrator: mocks.spawn }));
const coordinator = { id: "coordinator", workspaceId: "overall", workspaceName: "Coordinator storage", title: "Main conversation", kind: "orchestrator", status: "working", isTerminated: false };
beforeEach(() => {
 vi.clearAllMocks(); mocks.needsYou = false; mocks.remote = []; mocks.coordination.coordinatorProjectId = "overall";
 mocks.rows = [{ id: "overall", sessions: [coordinator] }, { id: "repo", sessions: [
  { id: "manager", workspaceId: "repo", workspaceName: "TEST repo", title: "Manager chat", kind: "orchestrator" },
  { id: "worker", workspaceId: "repo", workspaceName: "TEST repo", title: "Worker chat", kind: "worker" },
  { id: "remote", hostId: "remote-host", workspaceId: "repo", workspaceName: "Remote repo", title: "Remote chat", kind: "worker" },
 ] }];
});
it("reuses the existing persistent main conversation without spawning or reparenting", async () => {
 render(<WorkspaceEntry />);
 await userEvent.click(screen.getByRole("button", { name: "Open main conversation and board" }));
 expect(mocks.open).toHaveBeenCalledWith("overall", "coordinator");
 expect(mocks.spawn).not.toHaveBeenCalled();
});
it("starts the main conversation from Home when none is running, and says so when that fails", async () => {
 mocks.rows = [{ id: "overall", sessions: [] }];
 mocks.spawn.mockRejectedValueOnce(new Error("agent unavailable")).mockResolvedValueOnce("fresh");
 render(<WorkspaceEntry />);
 await userEvent.click(screen.getByRole("button", { name: "Open main conversation and board" }));
 expect(await screen.findByRole("alert")).toHaveTextContent("agent unavailable");
 expect(mocks.open).not.toHaveBeenCalled();
 await userEvent.click(screen.getByRole("button", { name: "Open main conversation and board" }));
 await vi.waitFor(() => expect(mocks.open).toHaveBeenCalledWith("overall", "fresh"));
 expect(mocks.spawn).toHaveBeenLastCalledWith("overall", "sidebar", false, "chat");
});
it("tells Ed on Home while the coordinator is waiting for his answer, and keeps Open main primary", async () => {
 mocks.needsYou = true;
 render(<WorkspaceEntry />);
 expect(screen.getByRole("status")).toHaveTextContent("The coordinator is waiting for your answer.");
 expect(screen.getByRole("button", { name: "Open main conversation and board" })).toBeEnabled();
});
it("shows no waiting line when nothing needs Ed", () => {
 render(<WorkspaceEntry />);
 expect(screen.queryByRole("status")).not.toBeInTheDocument();
});
it("opens coordination settings when the workspace has not been configured", async () => {
 mocks.coordination.coordinatorProjectId = "";
 const settings = vi.spyOn(useUiStore.getState(), "openGlobalSettings");
 render(<WorkspaceEntry />);
 await userEvent.click(screen.getByRole("button", { name: "Configure workspace coordination" }));
 expect(settings).toHaveBeenCalledWith("general"); settings.mockRestore();
});
it("lists coordinator, manager and worker directly under Team and preserves session destinations", async () => {
 mocks.remote = [{ id: "standalone", sessions: [{ id: "solo", workspaceId: "__standalone__", workspaceName: "Scratchpad", title: "Standalone chat", kind: "worker", hostId: "other-host" }] }];
 render(<TeamView />);
 expect(screen.getByRole("list", { name: "Team" })).toBeInTheDocument();
 expect(screen.getByText(/Coordinator ·/)).toBeInTheDocument();
 expect(screen.getByText(/Manager ·/)).toBeInTheDocument();
 await userEvent.click(screen.getByRole("button", { name: "Open Worker chat conversation" }));
 expect(mocks.open).toHaveBeenLastCalledWith("repo", "worker", undefined);
 await userEvent.click(screen.getByRole("button", { name: "Open Remote chat conversation" }));
 expect(mocks.open).toHaveBeenLastCalledWith("repo", "remote", "remote-host");
 await userEvent.click(screen.getByRole("button", { name: "Open Standalone chat conversation" }));
 expect(mocks.open).toHaveBeenLastCalledWith("__standalone__", "solo", "other-host");
 await userEvent.type(screen.getByLabelText("Find an agent"), "Manager chat");
 expect(screen.queryByText("Worker chat")).not.toBeInTheDocument();
 expect(screen.getByText("Manager chat")).toBeInTheDocument();
});
