import { useState } from "react";
import { useTranslation } from "react-i18next";
import { useCoordinationQuery } from "../hooks/useCoordinationQuery";
import { useWorkspaceQuery } from "../hooks/useWorkspaceQuery";
import { apiErrorMessage } from "../lib/api-client";
import { useNavigateToSession } from "../lib/navigate-to-session";
import { spawnOrchestrator } from "../lib/spawn-orchestrator";
import { useUiStore } from "../stores/ui-store";
import { newestActiveOrchestrator } from "../types/workspace";
import { Button } from "./ui/button";

export function WorkspaceEntry() {
	const { t } = useTranslation();
	const config = useCoordinationQuery();
	const workspace = useWorkspaceQuery({ includeCloud: false });
	const navigate = useNavigateToSession();
	const [busy, setBusy] = useState(false);
	const [error, setError] = useState("");
	const projectId = config.data?.coordinatorProjectId;
	async function open() {
		if (!projectId) return;
		setBusy(true); setError("");
		try {
			const row = workspace.data?.find(project => project.id === projectId);
			const existing = row && newestActiveOrchestrator(row.sessions);
			const id = existing?.id ?? await spawnOrchestrator(projectId, "sidebar", false, "chat");
			navigate(projectId, id);
		} catch (err) { setError(apiErrorMessage(err)); } finally { setBusy(false); }
	}
	return <main className="min-h-0 flex-1 overflow-auto p-6">
		<h1 className="text-xl font-semibold">{t("workspace.home")}</h1>
		<p className="my-4 text-muted-foreground">{t("workspace.homeDescription")}</p>
		{error || config.isError || workspace.isError ? <p role="alert">{error || t("workspace.teamError")}</p> : null}
		{projectId ? <Button disabled={busy || !workspace.isSuccess} onClick={() => void open()}>{t("workspace.openMain")}</Button> : <Button disabled={!config.isSuccess} onClick={() => useUiStore.getState().openGlobalSettings("general")}>{t("workspace.configure")}</Button>}
	</main>;
}
