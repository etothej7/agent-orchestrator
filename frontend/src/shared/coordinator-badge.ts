/** What the badge reads of one notification (GET /api/v1/notifications). */
export interface BadgeNotification {
	type: string;
	purpose?: string;
	status: string;
}

export interface BadgePage {
	/** The daemon's count of coordinator questions still waiting on Ed. */
	unresolvedAskCount?: number;
	notifications: readonly BadgeNotification[];
}

/**
 * The red Dock badge: open coordinator questions plus results and stops Ed has not seen. The open
 * count is the daemon's own; unread results and stops are counted from the notifications returned.
 */
export function coordinatorBadgeCount(page: BadgePage): number {
	const unreadTells = page.notifications.filter(
		(notification) =>
			notification.type === "coordinator_message" &&
			(notification.purpose === "verified_result" || notification.purpose === "stopped_work") &&
			notification.status === "unread",
	).length;
	return (page.unresolvedAskCount ?? 0) + unreadTells;
}
