import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { apiClient } from "../lib/api-client";
import { StoryBoard } from "./CoordinatorWorkspace";

const { sessionState, navigateSession } = vi.hoisted(() => ({
 sessionState: { sessions: [] as Array<{ id: string; workspaceId: string; title?: string; isTerminated?: boolean }> },
 navigateSession: vi.fn(),
}));
vi.mock("../hooks/useWorkspaceQuery", () => ({ useWorkspaceQuery: () => ({ data: [{ id: "repo", name: "TEST repo", sessions: sessionState.sessions }] }) }));
vi.mock("../lib/use-session-link-navigation", () => ({ useSessionLinkNavigation: () => navigateSession }));
const one = { id: "one", epicId: "a", title: "TEST first story", scope: "Agreed scope", projectIds: ["repo", "repo2"], assignments: [{ sessionId: "manager", role: "manager" }], status: "in_progress", blockedReason: "Waiting for fixture", question: "Which fixture?", questionRequesterSessionId:"manager", questionEventId:"question", revision: 3 };
const two = { ...one, id: "two", epicId: "b", title: "TEST second story" };
beforeEach(() => {
 sessionState.sessions = [{ id: "manager", workspaceId: "repo", title: "Alex Shared Manager" }];
 navigateSession.mockReset();
 vi.spyOn(apiClient, "GET").mockImplementation(async path => {
  if (path === "/api/v1/workspace") return { data: { epics: [{ id: "a", title: "TEST epic A" }, { id: "b", title: "TEST epic B" }], stories: [one, two] } } as never;
  if (path === "/api/v1/workspace/stories/{storyId}") return { data: { story: one, events: [{ id: "event", storyId: "one", kind: "report", senderSessionId: "manager", recipientSessionId: "", text: "Public worker result", reference: "rpt_1", result: "done", createdAt: "2026-10-23T12:00:00Z" }] } } as never;
  throw new Error(`Unexpected ${path}`);
 });
});
afterEach(() => vi.restoreAllMocks());
function mount() { const client = new QueryClient({ defaultOptions: { queries: { retry: false } } }); return { ...render(<QueryClientProvider client={client}><StoryBoard /></QueryClientProvider>), client }; }
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
it("shows actual report destination separately from claim and opens the native review run", async () => {
 vi.mocked(apiClient.GET).mockImplementation(async path => {
  if (path === "/api/v1/workspace") return {data:{epics:[],stories:[one]}} as never;
  return {data:{story:one,events:[{id:"report",kind:"report",storyId:"one",senderSessionId:"worker",recipientSessionId:"manager",text:"Worker claim",reference:"r",result:"done",createdAt:"2026-10-23T12:00:00Z"},{id:"native",kind:"native_review",storyId:"one",senderSessionId:"",recipientSessionId:"worker",reviewId:"reviewer-identity",reviewRunId:"fixed-run",text:"Independent public findings",reference:"fixed-sha",result:"approved",createdAt:"2026-10-23T12:00:00Z"}],reports:[{id:"r",state:"done",deliveryState:"pending",recipientProjectId:"repo",recipientSessionId:"manager",deliveryAttempts:1}]}} as never;
 });
 mount();await userEvent.click(await screen.findByText("TEST first story"));
 expect(await screen.findByText(/Report claim: done.*Outbox: pending/)).toBeInTheDocument();
 await userEvent.click(screen.getByRole("button",{name:"Native reviewer reviewer-identity"}));
 expect(screen.getByRole("region",{name:"Native review result"})).toHaveTextContent("fixed-run");
 expect(screen.getByRole("region",{name:"Native review result"})).toHaveTextContent("fixed-sha");
});

it("sends an Ed answer without requiring a manually selected destination", async () => {
 const post=vi.spyOn(apiClient,"POST").mockResolvedValue({data:{}} as never);
 mount();await userEvent.click(await screen.findByText("TEST first story"));
 await userEvent.selectOptions(await screen.findByLabelText("Story action"),"answer");
 expect(screen.queryByLabelText("Responsible agent")).not.toBeInTheDocument();
 await userEvent.type(screen.getByLabelText("Story message"),"Use the blue fixture");
 await userEvent.click(screen.getByRole("button",{name:"Send to agent"}));
 await waitFor(()=>expect(post).toHaveBeenCalledTimes(1));
 expect(post.mock.calls[0]?.[1]).toMatchObject({body:{kind:"answer",storyId:"one",text:"Use the blue fixture"}});
});

it("polls the original public reply, updates it in place, and preserves its source after reopening", async () => {
 let stage = 0;
 const reply = {id:"ede99368-5e6f-44f2-906b-1373d98f4264",conversationId:"conversation",turnId:"b6cc2933-8a75-45ff-950b-1789fa14daf3",senderSessionId:"manager",recipientSessionId:"",text:"Test reply",streaming:true,createdAt:"2026-10-24T20:25:34Z",updatedAt:"2026-10-24T20:25:34Z"};
 const get = vi.mocked(apiClient.GET).mockImplementation(async path => {
  if (path === "/api/v1/workspace") return {data:{epics:[],stories:[one]}} as never;
  return {data:{story:one,events:[{id:"original-exchange",storyId:one.id,kind:"message",senderSessionId:"",recipientSessionId:"manager",text:"Harmless workflow test",result:"delivered",createdAt:"2026-10-24T20:25:31Z"}],replyThreads:[{exchangeId:"original-exchange",attribution:"turn",messages:stage===0?[]:[{...reply,text:stage===1?"Test reply":"Test reply received",streaming:stage===1}]}]}} as never;
 });
 const post=vi.spyOn(apiClient,"POST");
 mount();await userEvent.click(await screen.findByText("TEST first story"));
 await screen.findByText("Harmless workflow test");
 expect(screen.queryByText("Test reply received")).not.toBeInTheDocument();
 stage=1;
 await screen.findByText("Test reply",{}, {timeout:3500});
 expect(screen.getByText(/Reply in progress/)).toBeInTheDocument();
 stage=2;
 await screen.findByText("Test reply received",{}, {timeout:3500});
 expect(screen.queryByText(/Reply in progress/)).not.toBeInTheDocument();
 expect(document.querySelectorAll(`[data-message-id="${reply.id}"]`)).toHaveLength(1);
 expect(screen.getByText(`Message: ${reply.id}`)).toBeInTheDocument();
 expect(screen.getByText(`Turn: ${reply.turnId}`)).toBeInTheDocument();
 expect(screen.getByText("In progress")).toBeInTheDocument();
 await userEvent.click(screen.getByRole("button",{name:"Back to stories"}));
 await userEvent.click(await screen.findByText("TEST first story"));
 expect(await screen.findByText("Test reply received")).toBeInTheDocument();
 expect(get.mock.calls.filter(call=>call[0]==="/api/v1/workspace/stories/{storyId}").length).toBeGreaterThanOrEqual(3);
 expect(post).not.toHaveBeenCalled();
},10000);

it("explains shared-turn attribution limits without presenting a reply", async () => {
 vi.mocked(apiClient.GET).mockImplementation(async path => {
  if(path==="/api/v1/workspace") return {data:{epics:[],stories:[one]}} as never;
  return {data:{story:one,events:[{id:"steer",storyId:one.id,kind:"message",senderSessionId:"",recipientSessionId:"manager",text:"Shared turn request",result:"delivered",createdAt:"2026-10-24T20:25:31Z"}],replyThreads:[{exchangeId:"steer",attribution:"unavailable",messages:[]}]}} as never;
 });
 mount();await userEvent.click(await screen.findByText("TEST first story"));
 expect(await screen.findByText(/cannot be attributed safely to this story/)).toBeInTheDocument();
 expect(screen.queryByText("Reply source")).not.toBeInTheDocument();
});


it("shows canonical name and story role visibly in cards, actor links, choices and answer destination", async () => {
 const label = "Alex Shared Manager · Manager (manager)";
 mount();
 expect(await screen.findByRole("button", { name: /TEST first story.*Alex Shared Manager · Manager \(manager\)/ })).toBeVisible();
 await userEvent.click(screen.getByText("TEST first story"));
 const activity = await screen.findByRole("list", { name: "Public story activity" });
 const actor = within(activity).getByRole("button", { name: label });
 expect(actor).toHaveTextContent(label);
 await userEvent.click(actor);
 expect(navigateSession).toHaveBeenCalledWith("ao://sessions/repo/manager");
 expect(within(screen.getByLabelText("Responsible agent")).getByRole("option", { name: label })).toBeVisible();
 await userEvent.selectOptions(screen.getByLabelText("Story action"), "answer");
 expect(screen.getByText(/^Destination:/)).toHaveTextContent(`Destination: ${label}`);
});

it("uses the current story role rather than inferring it from the session title", async () => {
 vi.mocked(apiClient.GET).mockImplementation(async path => {
  if (path === "/api/v1/workspace") return { data: { epics: [], stories: [one] } } as never;
  return { data: { story: { ...one, assignments: [{ sessionId: "manager", role: "reviewer" }] }, events: [] } } as never;
 });
 mount(); await userEvent.click(await screen.findByText("TEST first story"));
 expect(await screen.findByRole("button", { name: "Alex Shared Manager · Reviewer (manager)" })).toBeVisible();
});

it("refreshes renamed canonical titles while retaining the original navigation ID", async () => {
 const view = mount(); await userEvent.click(await screen.findByText("TEST first story"));
 await screen.findByText("Public worker result");
 sessionState.sessions = [{ id: "manager", workspaceId: "repo", title: "Renamed story manager" }];
 view.rerender(<QueryClientProvider client={view.client}><StoryBoard /></QueryClientProvider>);
 const activity = screen.getByRole("list", { name: "Public story activity" });
 const actor = within(activity).getByRole("button", { name: "Renamed story manager · Manager (manager)" });
 expect(actor).toBeVisible();
 expect(screen.queryByText(/Alex Shared Manager/)).not.toBeInTheDocument();
 await userEvent.click(actor);
 expect(navigateSession).toHaveBeenCalledWith("ao://sessions/repo/manager");
});

it.each([
 ["missing", [], "Agent · Manager (manager; Unavailable)", false],
 ["unnamed", [{ id: "manager", workspaceId: "repo", title: "" }], "Agent · Manager (manager)", true],
 ["terminated", [{ id: "manager", workspaceId: "repo", title: "Archived manager", isTerminated: true }], "Archived manager · Manager (manager; Terminated)", true],
] as const)("keeps truthful visible identity for a %s session", async (_case, sessions, label, linked) => {
 sessionState.sessions = sessions.map(session => ({ ...session }));
 mount(); await userEvent.click(await screen.findByText("TEST first story"));
 const activity = await screen.findByRole("list", { name: "Public story activity" });
 expect(activity).toHaveTextContent(label);
 expect(Boolean(within(activity).queryByRole("button", { name: label }))).toBe(linked);
 if (_case === "terminated") expect(within(screen.getByLabelText("Responsible agent")).queryByRole("option", { name: label })).not.toBeInTheDocument();
});
