import { useState } from "react";
import { useTranslation } from "react-i18next";
import { useDecideHumanRequest, type HumanRequestDTO } from "../hooks/useHumanRequests";
import { useUiStore } from "../stores/ui-store";
import { ConfirmDialog } from "./ConfirmDialog";
import { Button } from "./ui/button";

const KIND_KEYS = {
	auto_resume_lift: "humanRequest.kind.auto_resume_lift",
	conversation_settings: "humanRequest.kind.conversation_settings",
	provider_switch: "humanRequest.kind.provider_switch",
	pr_merge: "humanRequest.kind.pr_merge",
} as const;

const FIELD_KEYS = {
	model: "humanRequest.field.model",
	reasoningEffort: "humanRequest.field.reasoningEffort",
	fastMode: "humanRequest.field.fastMode",
	planMode: "humanRequest.field.planMode",
	harness: "humanRequest.field.harness",
	autoResume: "humanRequest.field.autoResume",
	pullRequest: "humanRequest.field.pullRequest",
	headCommit: "humanRequest.field.headCommit",
} as const;

function isKnown<T extends object>(map: T, key: string): key is Extract<keyof T, string> {
	return Object.hasOwn(map, key);
}

/**
 * A request that lowers a safety (lifts a protection) or does something that cannot be taken back (merges a pull
 * request, under the person's own GitHub identity) asks once more before it is approved, Cancel first.
 */
const CONFIRM_AGAIN = {
	auto_resume_lift: { title: "humanRequest.confirmLiftTitle", confirm: "humanRequest.confirmLift" },
	pr_merge: { title: "humanRequest.confirmMergeTitle", confirm: "humanRequest.confirmMerge" },
} as const;

function confirmAgainFor(request: HumanRequestDTO) {
	return Object.hasOwn(CONFIRM_AGAIN, request.kind) ? CONFIRM_AGAIN[request.kind as keyof typeof CONFIRM_AGAIN] : undefined;
}

function asksAgain(request: HumanRequestDTO): boolean {
	return confirmAgainFor(request) !== undefined;
}

/**
 * The requests waiting for the person. Every word about what a request does (the summary and the
 * before/after lines) was written by the daemon from the structured request, never by whoever filed it,
 * and is shown as received; only the labels around it are the app's own.
 */
export function HumanRequestCards({ requests }: { requests: HumanRequestDTO[] }) {
	const { t } = useTranslation();
	if (requests.length === 0) return null;
	return (
		<section aria-label={t("humanRequest.sectionLabel")} className="border-b border-border" data-testid="human-requests">
			<p className="px-4 pb-1 pt-3 text-caption font-medium text-muted-foreground">{t("humanRequest.title", { count: requests.length })}</p>
			<ul className="flex flex-col">
				{requests.map((request) => (
					<HumanRequestCard key={request.id} request={request} />
				))}
			</ul>
		</section>
	);
}

function HumanRequestCard({ request }: { request: HumanRequestDTO }) {
	const { t } = useTranslation();
	const decide = useDecideHumanRequest();
	const showGlobalToast = useUiStore((state) => state.showGlobalToast);
	const [confirming, setConfirming] = useState(false);
	const confirmAgain = confirmAgainFor(request);
	const busy = decide.isPending;
	const kindLabel = isKnown(KIND_KEYS, request.kind) ? t(KIND_KEYS[request.kind]) : request.kind;
	const requester =
		request.requester.kind === "session" && request.requester.sessionId
			? t("humanRequest.requestedBy", { requester: request.requester.sessionId })
			: t("humanRequest.requestedByTerminal");

	const approve = () =>
		decide.mutate(
			{ request, decision: "approve" },
			{
				onSuccess: () => {
					setConfirming(false);
					showGlobalToast(t("humanRequest.approved"), undefined, "top-center");
				},
			},
		);

	return (
		<li className="flex flex-col gap-2 border-t border-border px-4 py-3 first:border-t-0" data-request-id={request.id}>
			<div className="flex flex-col gap-0.5">
				<p className="text-control font-medium text-foreground">{kindLabel}</p>
				<p className="text-caption leading-4 text-muted-foreground">{requester}</p>
			</div>
			<p className="text-control leading-5 text-foreground">{request.summary}</p>
			{request.changes.length > 0 ? (
				<dl className="flex flex-col gap-0.5 text-caption leading-4">
					{request.changes.map((change) => (
						<div className="flex flex-wrap gap-x-1.5" key={change.field}>
							<dt className="text-muted-foreground">{isKnown(FIELD_KEYS, change.field) ? t(FIELD_KEYS[change.field]) : change.field}</dt>
							<dd className="font-mono text-foreground">{change.from ? `${change.from} → ${change.to}` : change.to}</dd>
						</div>
					))}
				</dl>
			) : null}
			{decide.isError ? (
				<p className="text-caption leading-4 text-error" role="alert">
					{decide.error.message || t("humanRequest.failed")}
				</p>
			) : null}
			<div className="flex gap-2">
				<Button
					aria-label={t("humanRequest.approveAria", { kind: kindLabel })}
					disabled={busy}
					onClick={() => (asksAgain(request) ? setConfirming(true) : approve())}
					size="sm"
					type="button"
					variant="primary"
				>
					{t("humanRequest.approve")}
				</Button>
				<Button
					aria-label={t("humanRequest.denyAria", { kind: kindLabel })}
					disabled={busy}
					onClick={() =>
						decide.mutate({ request, decision: "deny" }, { onSuccess: () => showGlobalToast(t("humanRequest.denied"), undefined, "top-center") })
					}
					size="sm"
					type="button"
					variant="outline"
				>
					{t("humanRequest.deny")}
				</Button>
			</div>
			{confirmAgain ? (
				<ConfirmDialog
					busy={busy}
					confirmLabel={t(confirmAgain.confirm)}
					description={request.summary}
					error={decide.isError ? decide.error.message || t("humanRequest.failed") : null}
					initialFocus="cancel"
					onConfirm={approve}
					onOpenChange={(open) => {
						if (!open) decide.reset();
						setConfirming(open);
					}}
					open={confirming}
					title={t(confirmAgain.title)}
				/>
			) : null}
		</li>
	);
}
