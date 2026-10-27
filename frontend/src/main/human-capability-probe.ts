import { createHmac, randomBytes, timingSafeEqual } from "node:crypto";

const PROBE_LABEL = "ao-ui-capability-probe:v2:";
const PROBE_PATH = "/internal/human-capability/probe";
const PROBE_TIMEOUT_MS = 2000;
/** A failed check may be retried (the daemon can be a moment from listening), but never in a tight loop. */
const RETRY_AFTER_MS = 1000;
const MAX_ATTEMPTS = 5;

/**
 * What a daemon holding `key` answers to `nonce` when the request reached it on `port` and it is process `pid`.
 * Mirror of uicapability.ProbeProof; pinned by the shared vectors. The port is the socket the daemon received the
 * request on, so a listener elsewhere cannot relay the nonce to the real daemon and pass its answer off as its own.
 */
export function probeProof(key: string, nonce: string, port: number, pid: number): string {
	return createHmac("sha256", key).update(`${PROBE_LABEL}${nonce}:${port}:${pid}`).digest("base64url");
}

/** The TCP port of an http origin, or undefined when it has none (nothing to bind a proof to). */
function portOf(origin: string): number | undefined {
	try {
		const port = Number(new URL(origin).port);
		return Number.isInteger(port) && port >= 1 && port <= 65535 ? port : undefined;
	} catch {
		return undefined;
	}
}

function sameProof(a: string, b: string): boolean {
	const left = Buffer.from(a);
	const right = Buffer.from(b);
	return left.length === right.length && timingSafeEqual(left, right);
}

export interface KeyPossessionOptions {
	key: string;
	fetchImpl?: typeof fetch;
	newNonce?: () => string;
	now?: () => number;
}

type Entry = { state: "pending"; attempts: number } | { state: "verified" } | { state: "failed"; attempts: number; retryAt: number };

/**
 * Proves that the daemon answering on an address holds this run's key before anything is signed for it.
 *
 * The address comes from places a same-user process can write (the run file the daemon is supposed to publish),
 * so being told "the daemon is on port N" is not enough to hand port N a valid capability. Before the first
 * signature the app sends a fresh random nonce and checks the answer: only the process the app gave the key to
 * can compute it, and the answer is an HMAC under its own label, so it reveals nothing and cannot be turned into
 * a capability. The proof also names the port the daemon received the probe on and its pid, and the app expects
 * the port it is about to sign for and the pid it spawned, so a listener that relays the nonce to the real daemon
 * only collects an answer for the wrong port. Nothing is signed while a check is pending or after one failed
 * (until a later retry succeeds).
 */
export function createKeyPossessionCheck(options: KeyPossessionOptions) {
	const doFetch = options.fetchImpl ?? fetch;
	const newNonce = options.newNonce ?? (() => randomBytes(24).toString("base64url"));
	const now = options.now ?? Date.now;
	const entries = new Map<string, Entry>();
	const keyOf = (origin: string, pid: number | undefined) => `${origin}#${pid ?? ""}`;

	async function check(id: string, origin: string, port: number, pid: number): Promise<void> {
		const nonce = newNonce();
		try {
			const response = await doFetch(`${origin}${PROBE_PATH}`, {
				method: "POST",
				headers: { "content-type": "application/json" },
				body: JSON.stringify({ nonce }),
				signal: AbortSignal.timeout(PROBE_TIMEOUT_MS),
			});
			if (!response.ok) throw new Error(`probe answered ${response.status}`);
			const body = (await response.json()) as { proof?: unknown };
			if (typeof body.proof !== "string" || !sameProof(body.proof, probeProof(options.key, nonce, port, pid))) throw new Error("probe proof did not match");
			entries.set(id, { state: "verified" });
		} catch {
			const previous = entries.get(id);
			const attempts = previous && previous.state !== "verified" ? previous.attempts : 1;
			entries.set(id, { state: "failed", attempts, retryAt: now() + RETRY_AFTER_MS });
		}
	}

	return {
		/** True only after this origin (for this daemon pid) answered a probe with the proof for its own port and pid. */
		isVerified(origin: string, pid: number | undefined): boolean {
			return entries.get(keyOf(origin, pid))?.state === "verified";
		},
		/** Starts a check if none is running, none succeeded, and a failed one is due a retry. Never throws. */
		ensure(origin: string, pid: number | undefined): void {
			const port = portOf(origin);
			if (port === undefined || pid === undefined || !Number.isInteger(pid) || pid < 1) return;
			const id = keyOf(origin, pid);
			const entry = entries.get(id);
			if (entry?.state === "pending" || entry?.state === "verified") return;
			if (entry?.state === "failed" && (entry.attempts >= MAX_ATTEMPTS || now() < entry.retryAt)) return;
			entries.set(id, { state: "pending", attempts: (entry?.state === "failed" ? entry.attempts : 0) + 1 });
			void check(id, origin, port, pid);
		},
	};
}
