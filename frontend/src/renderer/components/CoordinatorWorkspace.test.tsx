import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { apiClient } from "../lib/api-client";
import { StoryBoard } from "./CoordinatorWorkspace";

vi.mock("../hooks/useWorkspaceQuery", () => ({ useWorkspaceQuery: () => ({ data: [{ id: "repo", name: "TEST repo", sessions: [{ id: "manager", workspaceId: "repo" }] }] }) }));
vi.mock("../lib/use-session-link-navigation", () => ({ useSessionLinkNavigation: () => vi.fn() }));
const one = { id: "one", epicId: "a", title: "TEST first story", scope: "Agreed scope", projectIds: ["repo", "repo2"], assignments: [{ sessionId: "manager", role: "manager" }], status: "in_progress", blockedReason: "Waiting for fixture", question: "Which fixture?", revision: 3 };
const two = { ...one, id: "two", epicId: "b", title: "TEST second story" };
beforeEach(() => {
 vi.spyOn(apiClient, "GET").mockImplementation(async path => {
  if (path === "/api/v1/workspace") return { data: { epics: [{ id: "a", title: "TEST epic A" }, { id: "b", title: "TEST epic B" }], stories: [one, two] } } as never;
  if (path === "/api/v1/workspace/stories/{storyId}") return { data: { story: one, events: [{ id: "event", storyId: "one", kind: "report", senderSessionId: "manager", recipientSessionId: "", text: "Public worker result", reference: "rpt_1", result: "done", createdAt: "2026-10-23T12:00:00Z" }] } } as never;
  throw new Error(`Unexpected ${path}`);
 });
});
afterEach(() => vi.restoreAllMocks());
function mount() { const client = new QueryClient({ defaultOptions: { queries: { retry: false } } }); return render(<QueryClientProvider client={client}><StoryBoard /></QueryClientProvider>); }
it("filters epics and keeps card/detail status separate from report done", async () => {
 mount(); await screen.findByText("TEST first story");
 await userEvent.selectOptions(screen.getByLabelText("Filter by epic"), "a");
 expect(screen.queryByText("TEST second story")).not.toBeInTheDocument();
 await userEvent.click(screen.getByText("TEST first story"));
 expect(await screen.findByText("Public worker result")).toBeInTheDocument();
 expect(screen.getByText("In progress")).toBeInTheDocument();
 expect(screen.getByText("Blocked: Waiting for fixture")).toBeInTheDocument();
 expect(screen.getByText("Needs Ed: Which fixture?")).toBeInTheDocument();
});
it("routes the composer to the explicit recipient and story and retains uncertain retry identity", async () => {
 const post = vi.spyOn(apiClient, "POST").mockRejectedValueOnce(new Error("connection lost")).mockResolvedValue({ data: {} } as never);
 mount(); await userEvent.click(await screen.findByText("TEST first story"));
 await userEvent.selectOptions(await screen.findByLabelText("Responsible agent"), "manager");
 await userEvent.type(screen.getByLabelText("Story message"), "Please clarify the fixture");
 await userEvent.click(screen.getByRole("button", { name: "Send to agent" }));
 await screen.findByRole("alert");
 const first = post.mock.calls[0]?.[1];
 expect(first).toMatchObject({ body: { storyId: "one", recipientSessionId: "manager", senderSessionId: "", kind: "message", text: "Please clarify the fixture", revision: 3 } });
 await userEvent.click(screen.getByRole("button", { name: "Retry same request" }));
 await waitFor(() => expect(post).toHaveBeenCalledTimes(2));
 expect(post.mock.calls[1]?.[1]).toEqual(first);
});
