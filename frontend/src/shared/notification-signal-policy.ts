/**
 * Which OS-level attention signals a notification may raise. Ed's FINAL decision: none, ever: no banner,
 * no Dock bounce, no sound, no taskbar flash. They are permanently off and there is no setting or UI for
 * them. The signal code stays behind these constants only so it does not have to be deleted; they are
 * not a supported switch.
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
