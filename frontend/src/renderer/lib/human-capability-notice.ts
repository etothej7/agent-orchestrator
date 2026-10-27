import { appI18n } from "../i18n/instance";
import { useUiStore } from "../stores/ui-store";

/** The daemon's answer to a state-changing call it could not tie to the person using the app. */
export const HUMAN_CAPABILITY_REQUIRED_CODE = "HUMAN_CAPABILITY_REQUIRED";

/**
 * Tells the person that AO could not confirm an action came from them, in their words, once at a
 * time. The daemon's own message is written for an agent; this one is for the app's user. It means
 * the app's signed handoff to the background service did not take (for example the service was
 * started outside the app), and restarting AO is the way out.
 */
export function notifyHumanCapabilityRequired(): void {
	useUiStore.getState().showGlobalToast(appI18n.t("error.humanCapabilityRequired"), appI18n.t("error.humanCapabilityRequiredBody"), {
		tone: "error",
		dedupeKey: "human-capability-required",
	});
}
