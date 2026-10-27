import { randomBytes } from "node:crypto";
import type { WebContents } from "electron";
import { signUiRequestCapability, UI_CAPABILITY_HEADER } from "./ui-capability";

/** One random key per app run. It goes to the daemon over its private stdin pipe and nowhere else. */
export function newHumanCapabilityKey(): string {
	return randomBytes(32).toString("base64url");
}

/**
 * What the app writes to the daemon's private stdin pipe at spawn: the browser-runtime token on the
 * first line and the human-capability key on the second (the daemon's `ReadRuntimeHandoff` reads both).
 */
export function daemonStdinHandoff(browserRuntimeToken: string, uiCapabilityKey: string): string {
	return `${browserRuntimeToken}\n${uiCapabilityKey}\n`;
}

/** Marker env var for the daemon: "line two of stdin is the capability key". Carries no key. */
export const UI_CAPABILITY_STDIN_ENV = "AO_UI_CAPABILITY_STDIN";

/**
 * scheme://host[:port] of a URL. Not `URL.origin`: for a non-special scheme such as the packaged
 * renderer's app:// that is the string "null", which would make every such page look alike.
 */
export function originOf(url: string): string | undefined {
	if (!URL.canParse(url)) return undefined;
	const parsed = new URL(url);
	return `${parsed.protocol}//${parsed.host}`;
}

/**
 * The origin to sign for: the daemon this app run spawned itself, and only while it is ready. A daemon the app
 * merely found running (attached) never received this run's key, so signing for it would hand a valid capability
 * to whatever answers on that port. `spawnedPid` is the pid of the child this run started; the status pid on
 * the attach path comes from the run file and differs.
 */
export function ownDaemonOrigin(status: { state: string; port?: number; pid?: number }, spawnedPid: number | undefined): string | undefined {
	if (status.state !== "ready" || !status.port || spawnedPid === undefined || status.pid !== spawnedPid) return undefined;
	return `http://127.0.0.1:${status.port}`;
}

const STATE_CHANGING_METHODS: ReadonlySet<string> = new Set(["POST", "PUT", "PATCH", "DELETE"]);

type Frame = Pick<NonNullable<Electron.OnBeforeSendHeadersListenerDetails["frame"]>, "url" | "processId" | "routingId">;

export interface SendHeadersDetails {
	url: string;
	method: string;
	webContentsId?: number;
	frame?: Frame | null;
	requestHeaders: Record<string, string>;
}

type SendHeadersCallback = (response: { requestHeaders?: Record<string, string> }) => void;

export interface HeaderSession {
	webRequest: {
		onBeforeSendHeaders(filter: { urls: string[] }, listener: (details: SendHeadersDetails, callback: SendHeadersCallback) => void): void;
		onBeforeSendHeaders(listener: null): void;
	};
}

export interface HumanCapabilityHeaderOptions {
	session: HeaderSession;
	key: string;
	/** The trusted shell window. A request from any other web contents is never signed. */
	shellWebContents: () => Pick<WebContents, "id" | "mainFrame" | "isDestroyed"> | null;
	/** `http://127.0.0.1:<port>` while the daemon this app started is ready; undefined otherwise. */
	daemonOrigin: () => string | undefined;
	/** `originOf` the shell page itself (the packaged renderer's app://renderer, or the dev server). */
	shellPageOrigin: () => string;
	now?: () => number;
}

/**
 * Signs the state-changing requests the shell window sends to the daemon, and only those.
 *
 * The capability is added here in the main process, so the key never enters renderer
 * memory and no script has a "sign this" call to abuse. A request is signed only when ALL
 * hold: it changes state (POST, PUT, PATCH, DELETE), it goes to the daemon this app
 * started, it comes from the shell's own top-level page (not an iframe, not the browser
 * panel, not a preview), and that page is served from the app's own origin. The signature
 * binds the method and the escaped path exactly as sent, valid for 60 seconds. Any
 * capability header a page tried to set itself is dropped first.
 *
 * Whatever runs in the shell page holds human authority while it runs; the renderer keeps
 * untrusted agent content in sandboxed, different-origin frames for that reason.
 */
export function installHumanCapabilityHeaders(options: HumanCapabilityHeaderOptions): () => void {
	const now = options.now ?? Date.now;
	// Match every http request and decide in the listener: match patterns are unreliable about ports,
	// and the daemon's port changes on every restart. The packaged renderer is app://, so it never hits this.
	options.session.webRequest.onBeforeSendHeaders({ urls: ["http://*/*"] }, (details, callback) => {
		// This runs for every http request the shell session makes, and the request waits for `callback`. Anything
		// that throws here (a frame or web contents that is going away, a bad url, a throwing getter) must end in
		// "send it unsigned", never in a request that never completes: the daemon treats an unsigned write as not
		// the person's, which is the safe direction.
		let headers: Record<string, string>;
		try {
			headers = withoutCapability(details.requestHeaders);
			if (shouldSign(details, options)) {
				headers[UI_CAPABILITY_HEADER] = signUiRequestCapability(options.key, details.method, new URL(details.url).pathname, now());
			}
		} catch {
			headers = safeHeaders(details);
		}
		callback({ requestHeaders: headers });
	});
	return () => options.session.webRequest.onBeforeSendHeaders(null);
}

function withoutCapability(requestHeaders: Record<string, string>): Record<string, string> {
	const headers: Record<string, string> = {};
	for (const [name, value] of Object.entries(requestHeaders)) {
		if (name.toLowerCase() !== UI_CAPABILITY_HEADER.toLowerCase()) headers[name] = value;
	}
	return headers;
}

/** The request's own headers minus any capability, for the path where deciding failed; never throws. */
function safeHeaders(details: SendHeadersDetails): Record<string, string> {
	try {
		return withoutCapability(details.requestHeaders);
	} catch {
		return {};
	}
}

function shouldSign(details: SendHeadersDetails, options: HumanCapabilityHeaderOptions): boolean {
	if (!STATE_CHANGING_METHODS.has(details.method.toUpperCase())) return false;
	const daemon = options.daemonOrigin();
	if (!daemon || originOf(details.url) !== daemon) return false;
	const shell = options.shellWebContents();
	if (!shell || shell.isDestroyed() || details.webContentsId !== shell.id) return false;
	const frame = details.frame;
	const top = shell.mainFrame;
	if (!frame || !top || frame.processId !== top.processId || frame.routingId !== top.routingId) return false;
	const page = originOf(frame.url);
	return page !== undefined && page === options.shellPageOrigin();
}
