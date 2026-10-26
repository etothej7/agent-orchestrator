/**
 * Which OS-level attention signals a notification may raise. Ed's decision: none, ever. There is no
 * setting for these; flipping a constant here (and nowhere else) turns a signal back on.
 *
 * Attention reaches Ed through the red Dock badge (open coordinator questions and unread results and
 * stops) and the chat itself, never through a banner, a bounce, a sound or a taskbar flash.
 */
export interface NotificationSignalPolicy {
	/** Native notification banners (OS toasts). */
	readonly osBanners: boolean;
	/** The macOS Dock icon bouncing. */
	readonly dockBounce: boolean;
	/** AO's notification sound (and the OS chime that rides on a toast). */
	readonly sound: boolean;
	/** The Windows/Linux taskbar flash. */
	readonly taskbarFlash: boolean;
}

export const NOTIFICATION_SIGNAL_POLICY: NotificationSignalPolicy = {
	osBanners: false,
	dockBounce: false,
	sound: false,
	taskbarFlash: false,
};
