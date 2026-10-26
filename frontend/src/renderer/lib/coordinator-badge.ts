import { aoBridge } from "./bridge";

/** Asks the desktop to re-read the Dock badge now. Best effort: the desktop also polls on its own. */
export function refreshCoordinatorBadge(): void {
	try {
		aoBridge.coordinatorBadge?.refresh();
	} catch {
		// The poll will catch up.
	}
}
