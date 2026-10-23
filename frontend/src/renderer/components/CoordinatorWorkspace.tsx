import { useTranslation } from "react-i18next";
import { appI18n } from "../i18n/instance";
import { useState, type ReactNode } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import type { components } from "../../api/schema";
import { apiClient, apiErrorCode, apiErrorMessage } from "../lib/api-client";
import { useWorkspaceQuery } from "../hooks/useWorkspaceQuery";
import { useSessionLinkNavigation } from "../lib/use-session-link-navigation";
import { isOrchestratorSession, type WorkspaceSession } from "../types/workspace";
import { ChatMarkdown } from "./chat/ChatMarkdown";
import { Button } from "./ui/button";
import { Card, CardContent } from "./ui/card";

type Command = components["schemas"]["StoryCommandRequest"];
const statuses = [
 ["backlog", "workspace.backlog"], ["in_progress", "workspace.in_progress"], ["in_review", "workspace.in_review"],
 ["ready_for_approval", "workspace.ready_for_approval"], ["done", "workspace.done"],
] as const;
export function storyStatus(status: string) { const label = statuses.find(([id]) => id === status)?.[1]; return label ? appI18n.t(label) : status; }
const inputClass = "rounded-md border bg-background px-2 py-1.5 text-sm";

/** Keeps the existing overall-coordinator conversation mounted beside its shared board. */
export function CoordinatorWorkspace({ session, enabled, children }: { session?: WorkspaceSession; enabled: boolean; children: ReactNode }) {
 const config = useQuery({ queryKey: ["coordination"], enabled: enabled && Boolean(session && isOrchestratorSession(session)), queryFn: async () => {
  const { data, error } = await apiClient.GET("/api/v1/settings/coordination");
  if (error || !data) throw new Error(apiErrorMessage(error)); return data.coordination;
 }});
 const visible = enabled && session && isOrchestratorSession(session) && config.data?.coordinatorProjectId === session.workspaceId;
 return <div className="flex h-full min-h-0 min-w-0"><div className="relative min-h-0 min-w-0 flex-1">{children}</div>{visible ? <StoryBoard /> : null}</div>;
}

export function StoryBoard() {
 const { t } = useTranslation();
 const client = useQueryClient();
 const [epic, setEpic] = useState("");
 const [selected, setSelected] = useState("");
 const [error, setError] = useState("");
 const [busy, setBusy] = useState(false);
 const [epicTitle, setEpicTitle] = useState("");
 const [title, setTitle] = useState("");
 const [scope, setScope] = useState("");
 const [projects, setProjects] = useState<string[]>([]);
 const workspace = useWorkspaceQuery({ includeCloud: false });
 const board = useQuery({ queryKey: ["story-workspace"], refetchInterval: 2000, queryFn: async () => {
  const { data, error } = await apiClient.GET("/api/v1/workspace");
  if (error || !data) throw new Error(apiErrorMessage(error)); return data;
 }});
 const refresh = async () => { await client.invalidateQueries({ queryKey: ["story-workspace"] }); await client.invalidateQueries({ queryKey: ["story-detail"] }); };
 async function createEpic() {
  setBusy(true); setError("");
  try { const id = crypto.randomUUID(); const { error } = await apiClient.POST("/api/v1/workspace/epics", { body: { epic: { id, title: epicTitle } } }); if (error) throw new Error(apiErrorMessage(error)); setEpic(id); setEpicTitle(""); await refresh(); }
  catch (e) { setError(apiErrorMessage(e)); } finally { setBusy(false); }
 }
 async function createStory() {
  setBusy(true); setError("");
  try { const id = crypto.randomUUID(); const { error } = await apiClient.POST("/api/v1/workspace/stories", { body: { story: { id, epicId: epic, title, scope, projectIds: projects, assignments: [], status: "backlog", blockedReason: "", question: "", revision: 1 } } }); if (error) throw new Error(apiErrorMessage(error)); setSelected(id); setTitle(""); setScope(""); await refresh(); }
  catch (e) { setError(apiErrorMessage(e)); } finally { setBusy(false); }
 }
 return <aside aria-label={t("workspace.shared_story_workspace")} className="flex h-full w-[48%] min-w-[320px] max-w-[800px] flex-col border-l bg-background">
  <header className="space-y-2 border-b p-3"><h2 className="font-semibold">{t("workspace.shared_stories")}</h2><p className="text-xs text-muted-foreground">{t("workspace.the_coordinator_conversation_stays_open_stories_track_explicit_scope")}</p>
   <label className="flex items-center gap-2 text-sm">{t("workspace.epic")}<select aria-label={t("workspace.filter_by_epic")} className={inputClass} value={epic} onChange={e => { setEpic(e.target.value); setSelected(""); }}><option value="">{t("workspace.all_epics")}</option>{board.data?.epics?.map(e => <option key={e.id} value={e.id}>{e.title}</option>)}</select></label>
   <details><summary className="cursor-pointer text-sm">{t("workspace.create_epic_or_story")}</summary><div className="mt-2 flex flex-col gap-2">
    <input aria-label={t("workspace.epic_title")} className={inputClass} value={epicTitle} onChange={e => setEpicTitle(e.target.value)} placeholder={t("workspace.epic_title")} /><Button disabled={busy || !epicTitle.trim()} onClick={() => void createEpic()}>{t("workspace.create_epic")}</Button>
    <input aria-label={t("workspace.story_title")} className={inputClass} value={title} onChange={e => setTitle(e.target.value)} placeholder={t("workspace.story_title")} />
    <textarea aria-label={t("workspace.agreed_scope")} className={inputClass} value={scope} onChange={e => setScope(e.target.value)} placeholder={t("workspace.agreed_scope")} />
    <fieldset><legend className="text-sm">{t("workspace.repositories_in_scope")}</legend>{workspace.data?.filter(p => p.kind !== "scratch").map(p => <label key={p.id} className="mr-3 inline-flex items-center gap-1 text-sm"><input type="checkbox" checked={projects.includes(p.id)} onChange={e => setProjects(e.target.checked ? [...projects, p.id] : projects.filter(id => id !== p.id))} />{p.name}</label>)}</fieldset>
    <Button disabled={busy || !epic || !title.trim() || !projects.length} onClick={() => void createStory()}>{t("workspace.create_story_in_selected_epic")}</Button>
   </div></details>
  </header>
  {(error || board.error) ? <p role="alert" className="p-3 text-sm text-destructive">{error || apiErrorMessage(board.error)}</p> : null}
  <div className="min-h-0 flex-1 overflow-auto p-3">
   {selected ? <StoryDetail key={selected} id={selected} onBack={() => setSelected("")} onChanged={refresh} /> : <div className="space-y-4">{statuses.map(([status, label]) => <section key={status} aria-label={t(label)}><h3 className="mb-2 text-sm font-medium">{t(label)}</h3><div className="space-y-2">{board.data?.stories?.filter(s => (!epic || s.epicId === epic) && s.status === status).map(story => <Card key={story.id}><CardContent className="p-0"><button className="w-full space-y-1 p-3 text-left hover:bg-muted/40" onClick={() => setSelected(story.id)}><div className="font-medium">{story.title}</div><div className="text-xs text-muted-foreground">{story.projectIds?.join(" · ")}</div><div className="text-xs">{story.assignments?.map(a => `${a.role}: ${a.sessionId}`).join(" · ") || t("workspace.unassigned")}</div>{story.blockedReason ? <p className="text-sm text-destructive">{t("workspace.blocked",{reason:story.blockedReason})}</p> : null}{story.question ? <p className="text-sm">{t("workspace.needs_ed",{question:story.question})}</p> : null}</button></CardContent></Card>)}</div></section>)}</div>}
  </div>
 </aside>;
}

function StoryDetail({ id, onBack, onChanged }: { id: string; onBack: () => void; onChanged: () => Promise<void> }) {
 const { t } = useTranslation();
 const [nativeReview, setNativeReview] = useState("");
 const [kind, setKind] = useState("message");
 const [recipient, setRecipient] = useState("");
 const [role, setRole] = useState("worker");
 const [text, setText] = useState("");
 const [reference, setReference] = useState("");
 const [outcome, setOutcome] = useState("passed");
 const [pending, setPending] = useState<Command | null>(null);
 const [busy, setBusy] = useState(false);
 const [error, setError] = useState("");
 const navigateSession = useSessionLinkNavigation();
 const workspaces = useWorkspaceQuery({ includeCloud: false });
 const detail = useQuery({ queryKey: ["story-detail", id], refetchInterval: 2000, queryFn: async () => {
  const { data, error } = await apiClient.GET("/api/v1/workspace/stories/{storyId}", { params: { path: { storyId: id } } });
  if (error || !data) throw new Error(apiErrorMessage(error)); return data;
 }});
 const story = detail.data?.story;
 const sessions = workspaces.data?.flatMap(p => p.sessions) ?? [];
 function sessionLink(sessionId: string) {
  const session = sessions.find(s => s.id === sessionId);
  return session ? `ao://sessions/${encodeURIComponent(session.workspaceId)}/${encodeURIComponent(sessionId)}` : "";
 }
 function agent(sessionId: string) {
  if (!sessionId) return t("workspace.ed");
  const href = sessionLink(sessionId);
  return href ? <button className="underline" onClick={() => navigateSession(href)}>{sessionId}</button> : sessionId;
 }
 async function submit() {
  if (!story) return;
  const command = pending ?? { id: crypto.randomUUID(), storyId: id, revision: story.revision, kind, senderSessionId: "", recipientSessionId: recipient, text, reference, role, outcome };
  setPending(command); setBusy(true); setError("");
  try { const { error } = await apiClient.POST("/api/v1/workspace/commands", { body: command }); if (error) { if (["INVALID_STORY", "STORY_CHANGED", "STORY_KEY_REUSED"].includes(apiErrorCode(error) ?? "")) setPending(null); throw new Error(apiErrorMessage(error)); } setPending(null); setText(""); setReference(""); await onChanged(); }
  catch (e) {
   setError(apiErrorMessage(e));
   try {
    const receipt = await apiClient.GET("/api/v1/workspace/stories/{storyId}", { params: { path: { storyId: id } } });
    if (receipt.data?.events?.some(event => event.id === command.id && event.result === "failed")) setPending(null);
   } catch { /* Keep the original handle when receipt recovery is unavailable. */ }
  } finally { setBusy(false); }
 }
 if (!story) return <p role={detail.error ? "alert" : undefined}>{detail.error ? apiErrorMessage(detail.error) : t("workspace.loading_story")}</p>;
 const delivery = kind === "message" || kind === "assignment" || kind === "handoff" || kind === "answer";
 return <div className="space-y-4"><Button variant="ghost" onClick={onBack}>{t("workspace.back_to_stories")}</Button><div><h3 className="font-semibold">{story.title}</h3><p className="text-sm">{storyStatus(story.status)}</p><p className="whitespace-pre-wrap text-sm text-muted-foreground">{story.scope}</p><p className="text-xs">{story.projectIds?.join(" · ")}</p></div>
  {story.blockedReason ? <p className="text-sm text-destructive">{t("workspace.blocked",{reason:story.blockedReason})}</p> : null}{story.question ? <p className="text-sm">{t("workspace.needs_ed",{question:story.question})}</p> : null}
  <div className="space-y-1 text-sm">{story.assignments?.map(a => <p key={a.sessionId}>{a.role}: {agent(a.sessionId)}</p>)}</div>
  <ol aria-label={t("workspace.public_story_activity")} className="space-y-3">{detail.data?.events?.map(e => <li key={e.id} className="rounded-md border p-2 text-sm"><div className="text-xs text-muted-foreground">{e.kind} · {e.reviewId ? <button className="underline" onClick={() => setNativeReview(e.reviewRunId ?? "")}>{t("workspace.native_reviewer",{id:e.reviewId})}</button> : agent(e.senderSessionId)}{e.recipientSessionId ? <> → {agent(e.recipientSessionId)}</> : null} · <time dateTime={e.createdAt}>{new Date(e.createdAt).toLocaleString()}</time></div><ChatMarkdown text={e.text} /><p className="text-xs">{e.result === "delivered" ? t("workspace.delivered") : e.result}</p>{e.reference ? <p className="break-all text-xs">{t("workspace.result")}{e.reviewRunId ? <button className="underline" onClick={() => setNativeReview(e.reviewRunId ?? "")}>{t("workspace.review_result",{id:e.reviewRunId})} · {e.reference}</button> : e.recipientSessionId && sessionLink(e.recipientSessionId) ? <button className="underline" onClick={() => navigateSession(sessionLink(e.recipientSessionId))}>{e.reference}</button> : e.reference}</p> : null}{(e.result === "pending" || e.result === "uncertain") ? <Button size="sm" variant="outline" onClick={() => setPending({ id:e.id, storyId:id, revision:story.revision, kind:e.kind, senderSessionId:e.senderSessionId, recipientSessionId:e.recipientSessionId, text:e.text, reference:"", role:story.assignments?.find(a => a.sessionId === e.recipientSessionId)?.role ?? "", outcome:"" })}>{t("workspace.retry_same_request")}</Button> : null}{detail.data?.reports?.filter(r => r.id === e.reference).map(r => <p key={r.id} className="text-xs">{t("workspace.report_claim")}: {r.state || "informational"} · {t("workspace.outbox")}: {r.deliveryState} · {t("workspace.destination")}: {r.recipientSessionId ? agent(r.recipientSessionId) : t("workspace.recipient_pending",{id:r.recipientProjectId})} · {t("workspace.attempts")}: {r.deliveryAttempts ?? 0}</p>)}{detail.data?.reports?.find(r => r.id === e.reference)?.outputs?.map((output, index) => <div key={index} className="text-xs"><span>{output.kind}: </span><ChatMarkdown text={output.reference} /></div>)}</li>)}</ol>
  {nativeReview ? <section aria-label={t("workspace.native_review_result")} className="rounded-md border p-3">{detail.data?.events?.filter(e=>e.reviewRunId===nativeReview).map(e=><div key={e.id}><h4>{t("workspace.native_reviewer",{id:e.reviewId})}</h4><p>{t("workspace.review_revision",{id:e.reviewRunId,revision:e.reference})} · {e.result}</p><ChatMarkdown text={e.text}/></div>)}<Button onClick={()=>setNativeReview("")}>{t("workspace.close_review_result")}</Button></section> : null}
  <form className="space-y-2 border-t pt-3" onSubmit={e => { e.preventDefault(); void submit(); }}>
   <fieldset disabled={busy || Boolean(pending)} className="flex flex-col gap-2">
    <label className="text-sm">{t("workspace.action")}<select aria-label={t("workspace.story_action")} className={`${inputClass} ml-2`} value={kind} onChange={e => setKind(e.target.value)}>{["message", "assignment", "handoff", "blocked", "unblocked", "answer", "implementation", "testing", "in_progress", "in_review", "ready_for_approval", "done", "installation", "release"].map(k => <option key={k} value={k}>{k.replaceAll("_", " ")}</option>)}</select></label>
    {delivery && kind !== "answer" ? <select aria-label={t("workspace.responsible_agent")} className={inputClass} value={recipient} onChange={e => setRecipient(e.target.value)}><option value="">{t("workspace.choose_responsible_agent")}</option>{sessions.filter(s => !s.isTerminated && (kind === "assignment" ? story.projectIds?.includes(s.workspaceId) : story.assignments?.some(a => a.sessionId === s.id))).map(s => <option key={s.id} value={s.id}>{s.id}</option>)}</select> : null}
    {kind === "answer" ? <p>{t("workspace.destination")}: {story.questionRequesterSessionId ? agent(story.questionRequesterSessionId) : t("workspace.unassigned")}</p> : null}
    {kind === "assignment" ? <select aria-label={t("workspace.assignment_role")} className={inputClass} value={role} onChange={e => setRole(e.target.value)}>{["manager", "worker", "reviewer"].map(r => <option key={r}>{r}</option>)}</select> : null}
    <textarea aria-label={t("workspace.story_message")} className={inputClass} value={text} onChange={e => setText(e.target.value)} placeholder={delivery ? t("workspace.question_or_direction_delivered_to_the_selected_agent") : t("workspace.public_explanation_or_evidence")} required />
    {!delivery ? <input aria-label={t("workspace.evidence_reference")} className={inputClass} value={reference} onChange={e => setReference(e.target.value)} placeholder={t("workspace.fixed_revision_or_evidence_reference")} /> : null}
    {kind === "testing" ? <select aria-label={t("workspace.test_result")} className={inputClass} value={outcome} onChange={e => setOutcome(e.target.value)}><option value="passed">{t("workspace.passed")}</option><option value="failed">{t("workspace.failed")}</option></select> : null}
   </fieldset>
   {error ? <p role="alert" className="text-sm text-destructive">{error} {pending ? t("workspace.the_request_key_is_retained_for_a_safe_retry") : t("workspace.correct_the_request_and_try_again")}</p> : null}
   <Button type="submit" disabled={busy || (!pending && delivery && kind !== "answer" && !recipient) || (kind === "answer" && !story.questionRequesterSessionId)}>{busy ? t("workspace.recording") : pending ? t("workspace.retry_same_request") : delivery ? t("workspace.send_to_agent") : t("workspace.record_decision")}</Button>
   <p className="text-xs text-muted-foreground">{t("workspace.independent_reviewers_record_their_own_review_done_requires_explicit")}</p>
  </form>
 </div>;
}
