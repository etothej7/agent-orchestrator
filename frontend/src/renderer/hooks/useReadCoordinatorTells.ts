import { useEffect, useMemo, useRef, useState } from "react";
import { coordinatorMessageKind, notificationForMessage } from "../lib/coordinator-messages";
import { isUnreadCoordinatorTell } from "../lib/notifications";
import type { ConversationItem } from "../types/conversation";
import { useMarkAllNotificationsReadMutation, useNotificationsQuery } from "./useNotificationsQuery";

const windowIsFocused = () => typeof document !== "undefined" && document.visibilityState === "visible" && document.hasFocus();

/**
 * A coordinator result or stop counts toward the Dock badge until Ed has seen it. Seeing it means the
 * message is in the coordinator chat on screen and AO's window is focused; then it is marked read, and
 * the badge drops. It never prompts anything.
 */
export function useReadCoordinatorTells(enabled: boolean, onScreen: boolean, items: readonly ConversationItem[] | undefined): void {
	const history = useNotificationsQuery("all", enabled);
	const markRead = useMarkAllNotificationsReadMutation();
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
	const unreadSeen = useMemo(() => {
		if (!enabled || !items) return [];
		const tells = (history.data?.pages.flatMap((page) => page.notifications) ?? []).filter(
			(notification) => notification.type === "coordinator_message" && (notification.purpose === "verified_result" || notification.purpose === "stopped_work"),
		);
		const ids = new Set<string>();
		for (const item of items) {
			if (item.kind !== "message" || !coordinatorMessageKind(item) || coordinatorMessageKind(item) === "ask") continue;
			const notification = notificationForMessage(item, tells);
			if (notification && isUnreadCoordinatorTell(notification)) ids.add(notification.id);
		}
		return [...ids];
	}, [enabled, history.data, items]);
	const requested = useRef(new Set<string>());
	const { mutate } = markRead;
	useEffect(() => {
		if (!onScreen || !focused) return;
		const fresh = unreadSeen.filter((id) => !requested.current.has(id));
		if (fresh.length === 0) return;
		for (const id of fresh) requested.current.add(id);
		mutate(fresh, { onError: () => fresh.forEach((id) => requested.current.delete(id)) });
	}, [focused, mutate, onScreen, unreadSeen]);
}
