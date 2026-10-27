import { createHmac } from "node:crypto";

/**
 * Header the daemon reads a UI capability from. Must match backend/internal/uicapability.
 */
export const UI_CAPABILITY_HEADER = "X-AO-UI-Capability";
/** The one purpose every human-authored write is signed for. */
export const UI_CAPABILITY_PURPOSE_HUMAN_WRITE = "human-write";
const VERSION = "v1";

/**
 * Derive the capability secret from the per-launch runtime token. Mirror of
 * uicapability.DeriveSecret: its own label keeps this channel and the browser-runtime
 * channel from sharing a key. Pinned by a shared vector.
 */
export function deriveUiCapabilitySecret(runtimeToken: string): string {
	return createHmac("sha256", runtimeToken).update(`ao-ui-capability-secret:${VERSION}`).digest("base64url");
}

/** Subject a capability is bound to: the exact request it authorises. Mirror of uicapability.RequestSubject. */
export function uiCapabilityRequestSubject(method: string, path: string): string {
	return `${method.toUpperCase()} ${path}`;
}

/**
 * Sign a capability that proves a request came from AO's own desktop UI.
 *
 * `secret` is the per-launch secret the main process minted and handed to the daemon
 * over its private stdin pipe (never an environment variable or a file). The secret
 * itself is never sent: only an HMAC over purpose, subject and time, which the daemon
 * recomputes and checks within a 60 second window. Mirror of
 * backend/internal/uicapability.Sign: both sides are pinned by a shared test vector.
 */
export function signUiCapability(secret: string, purpose: string, subject: string, nowMs: number): string {
	const unix = Math.floor(nowMs / 1000);
	const mac = createHmac("sha256", secret)
		.update(`ao-ui-capability:${VERSION}:${purpose}:${subject}:${unix}`)
		.digest("base64url");
	return `${VERSION}.${unix}.${mac}`;
}

/** Capability for one request: purpose human-write, bound to method and path. */
export function signUiRequestCapability(secret: string, method: string, path: string, nowMs: number): string {
	return signUiCapability(secret, UI_CAPABILITY_PURPOSE_HUMAN_WRITE, uiCapabilityRequestSubject(method, path), nowMs);
}
