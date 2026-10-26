import { EyeOff } from "lucide-react";
import type { ReactNode } from "react";
import { useTranslation } from "react-i18next";
import type { ConversationItem } from "../../types/conversation";

/** What the coordinator's default view hides, as labelled by the daemon's unfiltered view. */
export function isInternalItem(item: ConversationItem): boolean {
	return item.audience === "internal";
}

export function isInternalGroup(items: readonly ConversationItem[]): boolean {
	return items.length > 0 && items.every(isInternalItem);
}

/**
 * Marks work Ed would not normally see. Every internal group is a named region for
 * assistive tech; the visible label shows once at the start of a run of consecutive
 * internal turns (`showLabel`). The label carries the meaning (muted text, 7.1:1 dark
 * and 5.5:1 light on the chat surface); the content keeps full contrast and every
 * control inside (edit, retry, expand) stays live.
 */
export function InternalActivity({ children, showLabel = true }: { children: ReactNode; showLabel?: boolean }) {
	const { t } = useTranslation();
	return (
		<div aria-label={t("chat.internalActivity")} className="flex min-w-0 flex-col gap-1" data-audience="internal" role="group">
			{showLabel ? (
				<span aria-hidden="true" className="flex items-center gap-1 text-caption text-muted-foreground">
					<EyeOff className="size-3" />
					{t("chat.internalActivity")}
				</span>
			) : null}
			<div className="min-w-0">{children}</div>
		</div>
	);
}
