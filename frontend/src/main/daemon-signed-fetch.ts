import { originOf } from "./human-capability";
import { UI_CAPABILITY_HEADER, signUiRequestCapability } from "./ui-capability";

const MUTATING: ReadonlySet<string> = new Set(["POST", "PUT", "PATCH", "DELETE"]);
const POLL_MS = 100;
/** How long a write waits for the daemon to prove it holds this run's key (the probe starts at port confirmation). */
export const DEFAULT_VERIFY_WAIT_MS = 8000;

export interface DaemonSignedFetchOptions {
	key: string;
	/**
	 * `http://127.0.0.1:<port>` of the daemon this app run spawned, once it has proven it holds this run's key; undefined
	 * before that, and for any daemon the app merely attached to. The same function the renderer-signing path uses, so main
	 * and the renderer sign for exactly the same daemon.
	 */
	daemonOrigin: () => string | undefined;
	fetchImpl?: typeof fetch;
	now?: () => number;
	sleep?: (ms: number) => Promise<void>;
}

/** Strips any capability header a caller set, so only this module ever attaches one. */
function withoutCapability(headers: HeadersInit | undefined): Headers {
	const out = new Headers(headers);
	out.delete(UI_CAPABILITY_HEADER);
	return out;
}

/**
 * The one way the Electron main process writes to the daemon. A state-changing request to the daemon this run
 * spawned carries the same per-request human capability the shell window's requests get (bound to method and
 * path, valid for 60 seconds, signed only for the daemon that proved it holds the key), because main acts for the
 * person too: it files the side-chat launch state, publishes downloads for Stay Awake, hands over the cloud user id
 * and shuts the daemon down on quit. Anything else is sent unchanged and unsigned: reads need no capability, and a
 * daemon that is not this run's own never receives one.
 *
 * A write that arrives before the daemon has proven itself (the first moments after it starts) waits for that,
 * up to `waitMs`, instead of going out unsigned and being refused. If it never proves itself the request is sent
 * unsigned and the daemon refuses it, which is the safe direction.
 */
export function createDaemonSignedFetch(options: DaemonSignedFetchOptions) {
	const doFetch = options.fetchImpl ?? ((input: string, init?: RequestInit) => fetch(input, init));
	const now = options.now ?? Date.now;
	const sleep = options.sleep ?? ((ms: number) => new Promise<void>((resolve) => setTimeout(resolve, ms)));

	return async function daemonFetch(url: string, init: RequestInit = {}, waitMs: number = DEFAULT_VERIFY_WAIT_MS): Promise<Response> {
		const method = (init.method ?? "GET").toUpperCase();
		if (!MUTATING.has(method)) return doFetch(url, init);
		const target = originOf(url);
		if (target === undefined) return doFetch(url, init);

		const deadline = now() + waitMs;
		let daemon = options.daemonOrigin();
		while (daemon === undefined && now() < deadline) {
			await sleep(POLL_MS);
			daemon = options.daemonOrigin();
		}
		const headers = withoutCapability(init.headers);
		if (daemon !== undefined && daemon === target) {
			headers.set(UI_CAPABILITY_HEADER, signUiRequestCapability(options.key, method, new URL(url).pathname, now()));
		}
		return doFetch(url, { ...init, headers });
	};
}
