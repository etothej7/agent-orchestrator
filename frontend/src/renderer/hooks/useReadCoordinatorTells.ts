import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { CoordinatorAskStatus } from "../components/chat/coordinator-asks";
import { coordinatorMessageKind, notificationForMessage } from "../lib/coordinator-messages";
import { refreshCoordinatorBadge } from "../lib/coordinator-badge";
import { isUnreadCoordinatorTell, type NotificationDTO } from "../lib/notifications";
import type { ConversationItem, ConversationMessage } from "../types/conversation";
import { useOpenAsks } from "./useCoordinatorAskStatus";
import { useMarkAllNotificationsReadMutation, useNotificationsQuery } from "./useNotificationsQuery";

const windowIsFocused = () => typeof document !== "undefined" && document.visibilityState === "visible" && document.hasFocus();

export interface CoordinatorUnread {
	/** A coordinator block entered or left the viewport (wired to the blocks through the chat context). */
	reportVisible: (messageId: string, visible: boolean) => void;
	/** Unread results/stops and open questions that are not in the viewport, oldest first. */
	outOfView: readonly string[];
	/**
	 * Message ids of unread results/stops and open questions of THIS conversation's session that the daemon
	 * still counts but that are OLDER than the loaded chat window (their message is not in the loaded pages),
	 * oldest first. Unread results come from the loaded notification pages (the first 100), so this can read
	 * lower than the Dock badge when more are unread than that; open questions come from the complete list.
	 */
	older: { ids: readonly string[] };
	/** The notification state needed to judge "nothing is waiting" has loaded. */
	settled: boolean;
}

/**
 * A coordinator result or stop counts toward the Dock badge until Ed has seen it. Seeing it means its
 * block has been in the viewport of the coordinator chat while AO's window is focused; then it is marked
 * read, and the badge drops. It never prompts anything. The same pass lists what still waits outside the
 * viewport (unread results and stops, open questions) so the chat can offer a way to it.
 */
export function useReadCoordinatorTells(
	enabled: boolean,
	onScreen: boolean,
	items: readonly ConversationItem[] | undefined,
	askStatusOf?: (message: ConversationMessage) => CoordinatorAskStatus,
	/** The session whose chat this is: only its own notifications can have a message in it. */
	sessionId?: string,
): CoordinatorUnread {
	const history = useNotificationsQuery("all", enabled);
	const markRead = useMarkAllNotificationsReadMutation();
	const openAsks = useOpenAsks(enabled, history.data?.pages[0]?.unresolvedAskCount);
	const [focused, setFocused] = useState(windowIsFocused);
	useEffect(() => {
		if (!enabled) return;
		const update = () => setFocused(windowIsFocused());
		window.addEventListener("focus", update);
		window.addEventListener("blur", update);
		document.addEventListener("visibilitychange", update);
		update();
		return () => {
			window.removeEventListener("focus", update);
			window.removeEventListener("blur", update);
			document.removeEventListener("visibilitychange", update);
		};
	}, [enabled]);
	const [visible, setVisible] = useState<ReadonlySet<string>>(() => new Set());
	const reportVisible = useCallback((messageId: string, isVisible: boolean) => {
		setVisible((current) => {
			if (current.has(messageId) === isVisible) return current;
			const next = new Set(current);
			if (isVisible) next.add(messageId);
			else next.delete(messageId);
			return next;
		});
	}, []);
	// Messages waiting for Ed, oldest first; tells carry the notification that marks them read.
	const waiting = useMemo(() => {
		const result: { messageId: string; notificationId?: string }[] = [];
		if (!enabled || !items) return result;
		const tells = (history.data?.pages.flatMap((page) => page.notifications) ?? []).filter(
			(notification) => notification.type === "coordinator_message" && (notification.purpose === "verified_result" || notification.purpose === "stopped_work"),
		);
		for (const item of items) {
			const kind = item.kind === "message" ? coordinatorMessageKind(item) : undefined;
			if (item.kind !== "message" || !kind) continue;
			if (kind === "ask") {
				// Like the block itself: a question nothing is known about is open.
				if (askStatusOf?.(item)?.open ?? true) result.push({ messageId: item.id });
				continue;
			}
			const notification = notificationForMessage(item, tells);
			if (notification && isUnreadCoordinatorTell(notification)) result.push({ messageId: item.id, notificationId: notification.id });
		}
		return result;
	}, [askStatusOf, enabled, history.data, items]);
	const requested = useRef(new Set<string>());
	const { mutate } = markRead;
	useEffect(() => {
		if (!onScreen || !focused) return;
		const fresh = waiting
			.filter((entry) => entry.notificationId && visible.has(entry.messageId) && !requested.current.has(entry.notificationId))
			.map((entry) => entry.notificationId as string);
		if (fresh.length === 0) return;
		for (const id of fresh) requested.current.add(id);
		mutate(fresh, {
			onSuccess: refreshCoordinatorBadge,
			onError: () => fresh.forEach((id) => requested.current.delete(id)),
		});
	}, [focused, mutate, onScreen, visible, waiting]);
	// An unread tell stays listed until it has been marked read, even if it scrolled out right after.
	const outOfView = useMemo(
		() => waiting.filter((entry) => !visible.has(entry.messageId) && !(entry.notificationId && requested.current.has(entry.notificationId))).map((entry) => entry.messageId),
		[visible, waiting],
	);
	// Server-side waiting items whose message is older than the loaded window: unread tells in the loaded
	// notification history and the daemon's complete list of open questions.
	const older = useMemo(() => {
		if (!enabled || !items || !sessionId) return { ids: [] } as CoordinatorUnread["older"];
		const loaded = new Set(items.filter((item) => item.kind === "message").map((item) => item.id));
		const unreadTells = (history.data?.pages.flatMap((page) => page.notifications) ?? []).filter(
			(notification) => notification.type === "coordinator_message" && (notification.purpose === "verified_result" || notification.purpose === "stopped_work") && isUnreadCoordinatorTell(notification),
		);
		const candidates = new Map<string, NotificationDTO>();
		for (const notification of [...unreadTells, ...(openAsks.data ?? [])]) {
			// A repo manager's chat has no coordinator blocks: only notifications addressed to this session count.
			if (notification.sessionId === sessionId && notification.messageId && !loaded.has(notification.messageId)) {
				candidates.set(notification.messageId, notification);
			}
		}
		const stamp = (notification: NotificationDTO) => Date.parse(notification.createdAt) || 0;
		const ids = [...candidates.values()]
			.sort((a, b) => stamp(a) - stamp(b) || (a.messageId ?? "").localeCompare(b.messageId ?? ""))
			.map((notification) => notification.messageId as string);
		return { ids };
	}, [enabled, history.data, items, openAsks.data, sessionId]);
	const settled = enabled && history.isSuccess && (openAsks.isSuccess || openAsks.isError) && items !== undefined;
	return { reportVisible, outOfView, older, settled };
}
