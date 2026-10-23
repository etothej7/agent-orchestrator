import { useState } from "react";
import { useTranslation } from "react-i18next";
import { useCoordinationQuery } from "../hooks/useCoordinationQuery";
import { useRemoteWorkspaces, useWorkspaceQuery } from "../hooks/useWorkspaceQuery";
import { useNavigateToSession } from "../lib/navigate-to-session";
import { isOrchestratorSession } from "../types/workspace";
import { Button } from "./ui/button";

/** A presentation of existing sessions; ownership and lifecycle remain with their repositories. */
export function TeamView() {
	const { t } = useTranslation();
	const workspace = useWorkspaceQuery();
	const coordination = useCoordinationQuery();
	const remote = useRemoteWorkspaces();
	const open = useNavigateToSession();
	const [search, setSearch] = useState("");
	const sessions = [...(workspace.data ?? []), ...remote.data].flatMap(row => row.sessions);
	const visible = sessions.filter(session => `${session.title} ${session.id} ${session.workspaceName}`.toLowerCase().includes(search.toLowerCase()));
	return <main className="min-h-0 flex-1 overflow-auto p-6">
		<h1 className="text-xl font-semibold">{t("workspace.team")}</h1>
		<p className="mb-4 text-sm text-muted-foreground">{t("workspace.teamDescription")}</p>
		<label className="flex flex-col gap-1 text-sm">{t("workspace.findAgent")}<input className="rounded border bg-background p-2" value={search} onChange={event => setSearch(event.target.value)} /></label>
		{workspace.isError || remote.failedHostIds.length > 0 ? <p role="alert">{t("workspace.teamError")}</p> : workspace.isLoading ? <p role="status">{t("workspace.loading")}</p> : null}
		<ul className="mt-4 space-y-2" aria-label={t("workspace.team")}>
			{visible.map(session => <li key={`${session.hostId ?? "local"}:${session.workspaceId}:${session.id}`} className="flex items-center justify-between gap-3 rounded border p-3">
				<div><p>{session.title || session.id}</p><p className="text-xs text-muted-foreground">{t(isOrchestratorSession(session) ? (!session.hostId && session.workspaceId === coordination.data?.coordinatorProjectId ? "workspace.coordinator" : "workspace.manager") : "workspace.agent")} · {session.workspaceName} · {session.id}</p></div>
				<Button variant="outline" onClick={() => open(session.workspaceId, session.id, session.hostId)} aria-label={t("workspace.openAgent", { name: session.title || session.id })}>{t("workspace.openConversation")}</Button>
			</li>)}
		</ul>
		{!workspace.isLoading && !workspace.isError && visible.length === 0 ? <p>{t("workspace.noAgents")}</p> : null}
	</main>;
}
