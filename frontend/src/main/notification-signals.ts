import { NOTIFICATION_SIGNAL_POLICY, type NotificationSignalPolicy } from "../shared/notification-signal-policy";

/**
 * Notification signal policy for the main process.
 *
 * The renderer forwards every `notification_created` event to `notifications:show`,
 * so the type reaching main is always one defined by
 * `backend/internal/domain/notification.go`. These helpers keep the decision of
 * "toast vs. active attention signal" in one typed, testable place rather than as
 * hardcoded string literals scattered through the IPC handler.
 */

/** The notification types defined by `backend/internal/domain/notification.go`. */
export type NotificationType = "needs_input" | "ready_to_merge" | "pr_merged" | "pr_closed_unmerged" | "coordinator_message";

/**
 * `coordinator_message` purposes that need Ed: the `ao ask` purposes, and work
 * that stopped (`ao tell --kind stopped`, which says "needs you"). A verified
 * result is the only coordinator message that is just good news.
 */
const COORDINATOR_URGENT_PURPOSES: ReadonlySet<string> = new Set([
	"scope_question",
	"blocking_decision",
	"completion_decision",
	"stopped_work",
]);

/**
 * Whether to fire the Windows/Linux taskbar flash. Restricted to the
 * actionable types: a merged or closed PR is worth a toast, but should not
 * demand attention as insistently as an agent blocked waiting on the user.
 *
 * The macOS dock bounce is deliberately NOT gated by this allowlist — every
 * notification bounces there, with urgency carried by {@link dockBounceType}.
 */
const ATTENTION_TYPES: ReadonlySet<string> = new Set<NotificationType>(["needs_input", "ready_to_merge", "coordinator_message"]);

/** Whether this notification type should flash the Windows/Linux taskbar. */
export function shouldSignalAttention(type: string | undefined): boolean {
	return type !== undefined && ATTENTION_TYPES.has(type);
}

/**
 * Whether a new macOS dock bounce should replace the pending one. A pending
 * "critical" bounce (agent blocked on the user) is never replaced: a later
 * informational notification must not downgrade it to a one-shot bounce, or
 * the blocked-agent signal would stop lasting until the user returns.
 */
export function shouldReplaceBounce(pending: { critical: boolean } | null): boolean {
	return pending === null || !pending.critical;
}

/**
 * Whether to fire an OS toast. Deliberately independent of the type list: every
 * backend notification type gets a toast, so adding a new type in
 * `notification.go` can never silently drop its toast (the bug this replaced).
 */
export function shouldToast(notification: { title?: string }, isSupported: boolean): boolean {
	return Boolean(notification.title) && isSupported;
}

/**
 * Platforms where Electron's Notification honours `silent`. The macOS
 * (cocoa_notification.mm) and Windows (windows_toast_notification.cc)
 * presenters read it; the Linux libnotify presenter never does and sends no
 * freedesktop `suppress-sound` hint, so on Linux the daemon alone decides
 * whether a toast chimes (Electron 33.4.11). The "Sound notifications"
 * preference therefore promises control over AO-owned audio everywhere, and
 * over the OS chime only where this returns true.
 */
export function osToastChimeControllable(platform: NodeJS.Platform): boolean {
	return platform === "darwin" || platform === "win32";
}

/**
 * The `silent` value for the OS toast, or `undefined` where the platform
 * ignores it (see osToastChimeControllable) so the call site cannot read a
 * muted toast into a value the presenter drops on the floor.
 *
 * Where it is honoured, two reasons mute: the user turned sound notifications
 * off (the preference has to silence every chime, not just ours), or AO is
 * about to play its own sound and must not layer the system chime on top of
 * it. Otherwise (sound on, informational type) the toast keeps its native
 * chime, which is the only sound that type gets.
 */
export function toastSilent(
	platform: NodeJS.Platform,
	soundNotificationsEnabled: boolean,
	playsSound: boolean,
): boolean | undefined {
	if (!osToastChimeControllable(platform)) return undefined;
	return !soundNotificationsEnabled || playsSound;
}

/**
 * macOS dock bounce style. A blocked agent waiting on the user keeps bouncing
 * until the app is activated ("critical"), and so does the coordinator asking Ed
 * for a decision or reporting stopped work; anything else bounces once
 * ("informational"). This is where urgency lives, so every notification can
 * signal without a merged PR or a verified result nagging as insistently as a
 * blocked agent.
 */
export function dockBounceType(type: string | undefined, purpose?: string): "critical" | "informational" {
	if (type === "needs_input") return "critical";
	return type === "coordinator_message" && purpose !== undefined && COORDINATOR_URGENT_PURPOSES.has(purpose) ? "critical" : "informational";
}

export type NotificationSignalPlan = {
	sound: boolean;
	banner: boolean;
	/** The macOS dock bounce to start, or null for none. */
	dockBounce: "critical" | "informational" | null;
	/** Flash the Windows/Linux taskbar. */
	flash: boolean;
};

/**
 * Every OS signal a notification may raise, decided in one place so the policy constants
 * (shared/notification-signal-policy.ts) are the only switch. With the default policy this is
 * always "nothing".
 */
export function planNotificationSignals(
	input: {
		type?: string;
		purpose?: string;
		title?: string;
		platform: NodeJS.Platform;
		/** The person is already looking at AO (window focused, or the prompt is on screen). */
		looking: boolean;
		soundNotificationsEnabled: boolean;
		toastSupported: boolean;
	},
	policy: NotificationSignalPolicy = NOTIFICATION_SIGNAL_POLICY,
): NotificationSignalPlan {
	const sound =
		policy.sound &&
		shouldSignalAttention(input.type) &&
		input.soundNotificationsEnabled &&
		(input.platform === "linux" || !input.looking);
	if (input.looking) return { sound, banner: false, dockBounce: null, flash: false };
	return {
		sound,
		banner: policy.osBanners && shouldToast({ title: input.title }, input.toastSupported),
		dockBounce: input.platform === "darwin" && policy.dockBounce ? dockBounceType(input.type, input.purpose) : null,
		flash: (input.platform === "win32" || input.platform === "linux") && policy.taskbarFlash && shouldSignalAttention(input.type),
	};
}
