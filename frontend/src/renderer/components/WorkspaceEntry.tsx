import { useTranslation } from "react-i18next";
import { useCoordinatorAskPending } from "../hooks/useCoordinatorAskPending";
import { useOpenMainCoordinator } from "../hooks/useOpenMainCoordinator";
import { useWorkspaceQuery } from "../hooks/useWorkspaceQuery";
import { useUiStore } from "../stores/ui-store";
import { Button } from "./ui/button";

export function WorkspaceEntry() {
	const { t } = useTranslation();
	const workspace = useWorkspaceQuery({ includeCloud: false });
	const main = useOpenMainCoordinator(workspace.data);
	const needsYou = useCoordinatorAskPending(main.projectId);
	return <main className="min-h-0 flex-1 overflow-auto p-6">
		<h1 className="text-xl font-semibold">{t("workspace.home")}</h1>
		<p className="my-4 text-muted-foreground">{t("workspace.homeDescription")}</p>
		{main.error || main.configError || workspace.isError ? <p role="alert">{main.error || t("workspace.teamError")}</p> : null}
		{needsYou ? <p role="status" className="mb-3 flex items-center gap-2 text-sm text-foreground"><span aria-hidden="true" className="size-2 shrink-0 rounded-full bg-status-needs-you" />{t("workspace.coordinatorWaiting")}</p> : null}
		{main.projectId ? <Button disabled={main.busy || !workspace.isSuccess} onClick={() => void main.open({ spawn: true })}>{t("workspace.openMain")}</Button> : <Button disabled={!main.configLoaded} onClick={() => useUiStore.getState().openGlobalSettings("general")}>{t("workspace.configure")}</Button>}
	</main>;
}
