import { useTranslation } from "react-i18next";
import { useAllowAutoResume } from "../hooks/useSessionAutoResume";
import { useUiStore } from "../stores/ui-store";
import { ConfirmDialog } from "./ConfirmDialog";

/**
 * Confirms lifting a session's `never` protection. The decision is Ed's alone, so it is made here, in the app:
 * the request carries the in-app confirmation marker and the daemon refuses it from the CLI or an agent.
 */
export function AllowAutoResumeDialog({
	open,
	onOpenChange,
	sessionId,
	sessionName,
}: {
	open: boolean;
	onOpenChange: (open: boolean) => void;
	sessionId: string;
	sessionName: string;
}) {
	const { t } = useTranslation();
	const showGlobalToast = useUiStore((state) => state.showGlobalToast);
	const allow = useAllowAutoResume(sessionId);
	return (
		<ConfirmDialog
			busy={allow.isPending}
			confirmLabel={t("session.allowAutoResumeConfirm")}
			description={t("session.allowAutoResumeBody", { name: sessionName })}
			error={allow.isError ? allow.error.message || t("session.allowAutoResumeFailed") : null}
			onConfirm={() =>
				allow.mutate(undefined, {
					onSuccess: () => {
						onOpenChange(false);
						showGlobalToast(t("session.allowAutoResumeDone"), undefined, "top-center");
					},
				})
			}
			onOpenChange={(next) => {
				if (!next) allow.reset();
				onOpenChange(next);
			}}
			open={open}
			title={t("session.allowAutoResumeTitle", { name: sessionName })}
		/>
	);
}
