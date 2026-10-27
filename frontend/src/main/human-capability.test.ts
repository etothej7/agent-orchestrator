import { describe, expect, it } from "vitest";
import { daemonStdinHandoff, installHumanCapabilityHeaders, newHumanCapabilityKey, originOf, type HeaderSession, type SendHeadersDetails } from "./human-capability";
import { signUiRequestCapability, UI_CAPABILITY_HEADER } from "./ui-capability";

const KEY = "k".repeat(43);
const DAEMON = "http://127.0.0.1:41234";
const PAGE_ORIGIN = "app://renderer";
const NOW = 1793102400_000;
const shellFrame = { url: `${PAGE_ORIGIN}/index.html`, processId: 7, routingId: 1 };
const shell = { id: 5, mainFrame: shellFrame, isDestroyed: () => false } as never;

type Details = SendHeadersDetails;

function capture(overrides: { shell?: unknown; daemonOrigin?: string | undefined } = {}) {
	let listener: ((details: Details, callback: (r: { requestHeaders?: Record<string, string> }) => void) => void) | null = null;
	const session = {
		webRequest: {
			onBeforeSendHeaders: (...args: unknown[]) => {
				listener = (args.length === 2 ? args[1] : args[0]) as typeof listener;
			},
		},
	} as unknown as HeaderSession;
	const dispose = installHumanCapabilityHeaders({
		session,
		key: KEY,
		shellWebContents: () => ("shell" in overrides ? (overrides.shell as never) : shell),
		daemonOrigin: () => ("daemonOrigin" in overrides ? overrides.daemonOrigin : DAEMON),
		shellPageOrigin: () => originOf(PAGE_ORIGIN) ?? "",
		now: () => NOW,
	});
	return {
		dispose,
		get listener() {
			return listener;
		},
		headersFor(details: Partial<Details>): Record<string, string> {
			let out: Record<string, string> = {};
			listener?.({ url: `${DAEMON}/api/v1/sessions/s-1/conversation/messages`, method: "POST", webContentsId: 5, frame: shellFrame, requestHeaders: { Origin: PAGE_ORIGIN }, ...details } as Details, (r) => {
				out = r.requestHeaders ?? {};
			});
			return out;
		},
	};
}

describe("installHumanCapabilityHeaders", () => {
	it("signs a state-changing request from the shell's own page to the daemon, bound to method and path", () => {
		const h = capture();
		for (const method of ["POST", "PUT", "PATCH", "DELETE"]) {
			const headers = h.headersFor({ method, url: `${DAEMON}/api/v1/sessions/s-1/auto-resume` });
			expect(headers[UI_CAPABILITY_HEADER]).toBe(signUiRequestCapability(KEY, method, "/api/v1/sessions/s-1/auto-resume", NOW));
			expect(headers.Origin).toBe(PAGE_ORIGIN);
		}
	});

	it("never puts the key in the header and signs the path exactly as sent", () => {
		const h = capture();
		const headers = h.headersFor({ url: `${DAEMON}/api/v1/sessions/s-1/artifact-files/a%20b/%E2%9C%93.txt?x=1` });
		expect(headers[UI_CAPABILITY_HEADER]).not.toContain(KEY);
		expect(headers[UI_CAPABILITY_HEADER]).toBe(signUiRequestCapability(KEY, "POST", "/api/v1/sessions/s-1/artifact-files/a%20b/%E2%9C%93.txt", NOW));
	});

	it.each(["GET", "HEAD", "OPTIONS"])("does not sign %s", (method) => {
		expect(capture().headersFor({ method })[UI_CAPABILITY_HEADER]).toBeUndefined();
	});

	it("does not sign for a request to any other origin (a remote host, a port that is not this daemon's, localhost)", () => {
		const h = capture();
		for (const url of ["http://192.168.1.9:3001/api/v1/x", "http://127.0.0.1:9/api/v1/x", "http://localhost:41234/api/v1/x", "https://127.0.0.1:41234/api/v1/x", "not a url"]) {
			expect(h.headersFor({ url })[UI_CAPABILITY_HEADER], url).toBeUndefined();
		}
	});

	it("does not sign while no daemon this app started is ready", () => {
		expect(capture({ daemonOrigin: undefined }).headersFor({})[UI_CAPABILITY_HEADER]).toBeUndefined();
	});

	it("does not sign for another web contents (the browser panel, a preview, a side window)", () => {
		expect(capture().headersFor({ webContentsId: 6 })[UI_CAPABILITY_HEADER]).toBeUndefined();
		expect(capture().headersFor({ webContentsId: undefined })[UI_CAPABILITY_HEADER]).toBeUndefined();
	});

	it("does not sign for a frame inside the shell (an agent page iframe) or a request with no frame", () => {
		const h = capture();
		expect(h.headersFor({ frame: { url: "https://agent.example/render.html", processId: 9, routingId: 4 } })[UI_CAPABILITY_HEADER]).toBeUndefined();
		expect(h.headersFor({ frame: { ...shellFrame, routingId: 2 } })[UI_CAPABILITY_HEADER]).toBeUndefined();
		expect(h.headersFor({ frame: null })[UI_CAPABILITY_HEADER]).toBeUndefined();
		expect(h.headersFor({ frame: undefined })[UI_CAPABILITY_HEADER]).toBeUndefined();
	});

	it("does not sign when the top frame is not served from the app's own origin", () => {
		const h = capture();
		const foreign = { ...shellFrame, url: "https://evil.example/index.html" };
		const foreignShell = { id: 5, mainFrame: foreign, isDestroyed: () => false } as never;
		expect(capture({ shell: foreignShell }).headersFor({ frame: foreign })[UI_CAPABILITY_HEADER]).toBeUndefined();
		expect(h.headersFor({ frame: { ...shellFrame, url: "about:blank" } })[UI_CAPABILITY_HEADER]).toBeUndefined();
	});

	it("does not sign without a live shell", () => {
		expect(capture({ shell: null }).headersFor({})[UI_CAPABILITY_HEADER]).toBeUndefined();
		expect(capture({ shell: { id: 5, mainFrame: shellFrame, isDestroyed: () => true } }).headersFor({})[UI_CAPABILITY_HEADER]).toBeUndefined();
	});

	it("drops a capability header a page set itself, in any case, on signed and unsigned requests alike", () => {
		const h = capture();
		const forged = { Origin: PAGE_ORIGIN, "x-ao-ui-capability": "v1.1.forged", [UI_CAPABILITY_HEADER]: "v1.1.forged", Accept: "*/*" };
		const signed = h.headersFor({ requestHeaders: forged });
		expect(Object.keys(signed).filter((name) => name.toLowerCase() === UI_CAPABILITY_HEADER.toLowerCase())).toEqual([UI_CAPABILITY_HEADER]);
		expect(signed[UI_CAPABILITY_HEADER]).toBe(signUiRequestCapability(KEY, "POST", "/api/v1/sessions/s-1/conversation/messages", NOW));
		expect(signed.Accept).toBe("*/*");
		const unsigned = h.headersFor({ method: "GET", requestHeaders: forged });
		expect(Object.keys(unsigned).some((name) => name.toLowerCase() === UI_CAPABILITY_HEADER.toLowerCase())).toBe(false);
	});

	it("unsubscribes on dispose", () => {
		const h = capture();
		expect(h.listener).not.toBeNull();
		h.dispose();
		expect(h.listener).toBeNull();
	});
});

describe("originOf", () => {
	it("keeps scheme and host for non-special schemes (URL.origin is the string null for app://)", () => {
		expect(originOf("app://renderer/index.html")).toBe("app://renderer");
		expect(originOf("http://127.0.0.1:41234/api/v1/x?y=1")).toBe("http://127.0.0.1:41234");
		expect(originOf("https://evil.example/index.html")).toBe("https://evil.example");
		expect(originOf("not a url")).toBeUndefined();
	});

	it("does not treat two different app:// pages as the same", () => {
		expect(originOf("app://other/index.html")).not.toBe(originOf("app://renderer/index.html"));
	});
});

describe("newHumanCapabilityKey", () => {
	it("is 256 random bits as base64url, different every time", () => {
		const a = newHumanCapabilityKey();
		expect(a).toMatch(/^[A-Za-z0-9_-]{43}$/);
		expect(newHumanCapabilityKey()).not.toBe(a);
	});
});

describe("daemonStdinHandoff", () => {
	it("is the token, then the key, one per line, ending in a newline (the shape the daemon's reader takes)", () => {
		const key = newHumanCapabilityKey();
		expect(daemonStdinHandoff("tok-1", key)).toBe(`tok-1\n${key}\n`);
		expect(daemonStdinHandoff("tok-1", key).split("\n")).toEqual(["tok-1", key, ""]);
	});
});
