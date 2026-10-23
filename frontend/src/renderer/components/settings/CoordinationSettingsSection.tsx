import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { apiClient, apiErrorMessage } from "../../lib/api-client";
import { spawnOrchestrator } from "../../lib/spawn-orchestrator";
import { sessionNavigateTarget } from "../../lib/navigate-to-session";
import { useSettings } from "../../hooks/useSettings";
import { useUiStore } from "../../stores/ui-store";
import { Button } from "../ui/button";
import { SettingsSection } from "./SettingsSection";
import { SettingsRow } from "./SettingsRow";
import { SettingsOptionMenu } from "./SettingsOptionMenu";

const queryKey = ["coordination"] as const;
const projectsKey = ["coordination-projects"] as const;

export function CoordinationSettingsSection() {
	const { t } = useTranslation();
	const client = useQueryClient();
	const navigate = useNavigate();
	const { settings } = useSettings();
	const [coordinator, setCoordinator] = useState("");
	const [managed, setManaged] = useState<string[]>([]);
	const [agent, setAgent] = useState("codex");
	const [dirty, setDirty] = useState(false);
	const [busy, setBusy] = useState(false);
	const [error, setError] = useState("");
	const [notice, setNotice] = useState("");
	const config = useQuery({
		queryKey,
		queryFn: async () => {
			const { data, error } = await apiClient.GET("/api/v1/settings/coordination");
			if (error || !data) throw new Error(apiErrorMessage(error, t("settings.coordination.loadError")));
			return data.coordination;
		},
	});
	const projects = useQuery({
		queryKey: projectsKey,
		queryFn: async () => {
			const { data, error } = await apiClient.GET("/api/v1/projects");
			if (error || !data) throw new Error(apiErrorMessage(error, t("settings.coordination.projectsError")));
			return data.projects;
		},
	});
	useEffect(() => {
		if (!dirty && config.data) {
			setCoordinator(config.data.coordinatorProjectId ?? "");
			setManaged(config.data.managedProjectIds ?? []);
		}
	}, [config.data, dirty]);
	const run = async (action: () => Promise<void>) => {
		if (busy) return;
		setBusy(true); setError(""); setNotice("");
		try { await action(); } catch (cause) { setError(apiErrorMessage(cause)); }
		finally { setBusy(false); }
	};
	const save = () => run(async () => {
		const { data, error } = await apiClient.PUT("/api/v1/settings/coordination", {
			body: { coordinatorProjectId: coordinator, managedProjectIds: coordinator ? managed : [] },
		});
		if (error || !data) throw new Error(apiErrorMessage(error, t("settings.coordination.saveError")));
		client.setQueryData(queryKey, data.coordination);
		setDirty(false);
		setNotice(t("settings.coordination.saved"));
	});
	const create = () => run(async () => {
		const { data, error } = await apiClient.POST("/api/v1/settings/coordination/project", { body: { agent } });
		if (error || !data) throw new Error(apiErrorMessage(error, t("settings.coordination.createError")));
		await client.invalidateQueries({ queryKey: projectsKey });
		await client.invalidateQueries({ queryKey: ["workspace"] });
		setCoordinator(data.projectId); setDirty(true);
		setNotice(t("settings.coordination.created"));
	});
	const open = () => run(async () => {
		const id = config.data?.coordinatorProjectId;
		if (!id) return;
		const sessionId = await spawnOrchestrator(id, "settings", false, "chat");
		useUiStore.getState().closeSettings();
		await navigate(sessionNavigateTarget(id, sessionId));
	});
	const ready = !!config.data && !!projects.data && !busy;
	const scratch = projects.data?.filter((p) => p.kind === "scratch") ?? [];
	const repos = projects.data?.filter((p) => p.kind !== "scratch") ?? [];
	const loadError = config.error ?? projects.error;
	const availableAgents = settings?.chatHarnesses ?? [];
	return (
		<SettingsSection title={t("settings.coordination.title")} sectionId="coordination" grouped>
			<div className="settings-content-row px-3 py-3 text-sm text-muted-foreground">
				{t("settings.coordination.description")}
			</div>
			<SettingsRow label={t("settings.coordination.coordinator")}>
				<SettingsOptionMenu aria-label={t("settings.coordination.coordinator")} value={coordinator} disabled={!ready}
					options={[{ value: "", label: t("settings.coordination.off") }, ...scratch.map((p) => ({ value: p.id, label: p.name }))]}
					onChange={(value) => { setCoordinator(value); setDirty(true); setNotice(""); }} />
			</SettingsRow>
			{!scratch.some((p) => p.id === "overall-coordinator") && !projects.isPending ? (
				<SettingsRow label={t("settings.coordination.createLabel")} description={t("settings.coordination.createDescription")}>
					<div className="flex flex-wrap items-center gap-2">
						<SettingsOptionMenu aria-label={t("settings.coordination.agent")} value={agent} disabled={!ready}
							options={availableAgents.map((id) => ({ value: id, label: id }))} onChange={setAgent} />
						<Button variant="outline" disabled={!ready || !availableAgents.includes(agent)} onClick={() => void create()}>{t("settings.coordination.create")}</Button>
					</div>
				</SettingsRow>
			) : null}
			{coordinator ? (
				<div className="settings-content-row space-y-3 px-3 py-3">
					<p className="text-sm font-medium">{t("settings.coordination.repos")}</p>
					{repos.length === 0 ? <p className="text-sm text-muted-foreground">{t("settings.coordination.addRepos")}</p> : null}
					{repos.map((p) => (
						<div key={p.id} className="flex items-center justify-between gap-3">
							<label className="flex min-w-0 items-center gap-2 text-sm">
								<input type="checkbox" checked={managed.includes(p.id)} disabled={!ready || !!p.resolveError || p.folderMissing}
									onChange={(event) => { setManaged((ids) => event.target.checked ? [...ids, p.id] : ids.filter((id) => id !== p.id)); setDirty(true); setNotice(""); }} />
								<span className="truncate">{p.name}</span>
							</label>
							{!p.orchestratorAgent ? <Button size="sm" variant="ghost" onClick={() => useUiStore.getState().openProjectSettings(p.id)}>{t("settings.coordination.chooseAgent")}</Button> : <span className="text-xs text-muted-foreground">{p.orchestratorAgent}</span>}
						</div>
					))}
				</div>
			) : null}
			<div className="settings-content-row space-y-3 px-3 py-3">
				{loadError || error ? <p role="alert" className="text-sm text-error">{error || apiErrorMessage(loadError)}</p> : null}
				{notice ? <p role="status" className="text-sm text-muted-foreground">{notice}</p> : null}
				<div className="flex flex-wrap gap-2">
					<Button disabled={!ready || !dirty} onClick={() => void save()}>{t("settings.coordination.save")}</Button>
					<Button variant="outline" disabled={!ready || dirty || !config.data?.coordinatorProjectId} onClick={() => void open()}>{t("settings.coordination.open")}</Button>
				</div>
			</div>
		</SettingsSection>
	);
}
