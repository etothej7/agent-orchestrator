import { CheckCheck, CircleAlert, MessageSquareDot } from "lucide-react";
import { useContext, useEffect, useRef } from "react";
import { useTranslation } from "react-i18next";
import { coordinatorMessageKind, type CoordinatorMessageKind } from "../../lib/coordinator-messages";
import type { ConversationMessage } from "../../types/conversation";
import { Button } from "../ui/button";
import { ChatMarkdown } from "./ChatMarkdown";
import { CoordinatorAskContext } from "./coordinator-asks";

/** In view once a quarter of the block shows, or (for a block taller than the viewport) half a viewport of it. */
const SEEN_RATIO = 0.25;
const SEEN_VIEWPORT_FRACTION = 0.5;
// Fine steps, so a block much taller than the viewport still reports as more of it scrolls in.
const SEEN_THRESHOLDS = Array.from({ length: 21 }, (_, step) => step / 20);

const ICON = { result: CheckCheck, stopped: CircleAlert, ask: MessageSquareDot } as const;

/**
 * A message the coordinator addressed to Ed on purpose (a result, stopped work, or a question). It
 * is always a labelled block, never a bare line, and stays where it happened in the conversation.
 * A question offers Answer while it is open and reads as settled once answered.
 */
export function CoordinatorMessageBlock({ message }: { message: ConversationMessage }) {
	const { t } = useTranslation();
	const controls = useContext(CoordinatorAskContext);
	const kind = coordinatorMessageKind(message) as CoordinatorMessageKind;
	const Icon = ICON[kind];
	const status = kind === "ask" ? controls?.statusOf?.(message) : undefined;
	// Without information a question is treated as open: the daemon validates the answer.
	const open = kind === "ask" ? status?.open ?? true : false;
	const label = kind === "result" ? t("chat.coordinator.result") : kind === "stopped" ? t("chat.coordinator.stopped") : t("chat.coordinator.ask");
	const answering = controls?.answeringId === message.id;
	const element = useRef<HTMLElement>(null);
	const reportVisible = controls?.reportVisible;
	useEffect(() => {
		if (!reportVisible) return;
		const node = element.current;
		// Without IntersectionObserver (tests) a mounted block counts as in view.
		if (!node || typeof IntersectionObserver === "undefined") {
			reportVisible(message.id, true);
			return () => reportVisible(message.id, false);
		}
		// Observed against the chat scroller (its viewport), not the window.
		const observer = new IntersectionObserver((entries) => {
			for (const entry of entries) {
				const viewport = entry.rootBounds?.height ?? 0;
				const tallSlice = viewport > 0 && (entry.intersectionRect?.height ?? 0) >= viewport * SEEN_VIEWPORT_FRACTION;
				reportVisible(message.id, entry.intersectionRatio >= SEEN_RATIO || tallSlice);
			}
		}, { root: node.closest<HTMLElement>("[role=log]"), threshold: SEEN_THRESHOLDS });
		observer.observe(node);
		return () => {
			observer.disconnect();
			reportVisible(message.id, false);
		};
	}, [message.id, reportVisible]);
	return (
		<section
			ref={element}
			aria-label={`${t("chat.coordinator.from")}: ${label}`}
			className="flex min-w-0 flex-col gap-1.5 rounded-md bg-secondary px-3 py-2.5"
			data-coordinator-message={kind}
			data-coordinator-message-id={message.id}
		>
			<div className="flex min-w-0 items-center gap-1.5 text-caption text-muted-foreground">
				<Icon
					aria-hidden="true"
					className={open || kind === "stopped" ? "size-3.5 shrink-0 text-warning" : kind === "result" ? "size-3.5 shrink-0 text-success" : "size-3.5 shrink-0"}
				/>
				<span>{t("chat.coordinator.from")}</span>
				<span aria-hidden="true">·</span>
				<span className="font-medium text-foreground">{label}</span>
				{kind === "ask" && !open ? (
					<span className="ml-auto shrink-0">
						{status?.resolution === "coordinator" ? t("chat.coordinator.resolvedByCoordinator") : t("chat.coordinator.answered")}
					</span>
				) : null}
			</div>
			<div className="min-w-0 text-foreground">
				<ChatMarkdown text={message.text} />
			</div>
			{kind === "ask" && open && controls ? (
				<div className="flex items-center gap-2">
					<Button
						aria-pressed={answering}
						onClick={() => controls.answer(message)}
						size="sm"
						variant={answering ? "ghost" : "primary"}
					>
						{t("chat.coordinator.answer")}
					</Button>
				</div>
			) : null}
		</section>
	);
}
