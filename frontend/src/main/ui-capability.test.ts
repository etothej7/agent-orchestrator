import { describe, expect, it } from "vitest";
import {
	deriveUiCapabilitySecret,
	signUiCapability,
	signUiRequestCapability,
	UI_CAPABILITY_HEADER,
	UI_CAPABILITY_PURPOSE_HUMAN_WRITE,
	uiCapabilityRequestSubject,
} from "./ui-capability";

describe("signUiCapability", () => {
	// Fixed vector shared with backend/internal/uicapability (TestSharedVectorWithTheElectronSigner).
	it("matches the daemon's signer byte for byte", () => {
		expect(
			signUiRequestCapability("test-secret", "POST", "/api/v1/sessions/sess-1/auto-resume/lift", 1793102400_000),
		).toBe("v1.1793102400.8VJhn2Mul_MHx3kNbKsRwyjuZlHU3YmE7pkfdvRUMos");
	});

	it("derives the same secret as the daemon", () => {
		expect(deriveUiCapabilitySecret("test-token")).toBe("TFmczbvMaO3L6eBh9WJalDFprS--z2nHeaXiUAiKO7U");
	});

	it("binds the secret, purpose, subject and second", () => {
		const p = UI_CAPABILITY_PURPOSE_HUMAN_WRITE;
		const base = signUiCapability("s", p, "a", 1_000_000);
		expect(signUiCapability("t", p, "a", 1_000_000)).not.toBe(base);
		expect(signUiCapability("s", "other", "a", 1_000_000)).not.toBe(base);
		expect(signUiCapability("s", p, "b", 1_000_000)).not.toBe(base);
		expect(signUiCapability("s", p, "a", 1_001_000)).not.toBe(base);
		// within the same second the value is stable
		expect(signUiCapability("s", p, "a", 1_000_999)).toBe(base);
	});

	it("binds the method and path of the request", () => {
		const base = signUiRequestCapability("s", "POST", "/a", 1_000_000);
		expect(signUiRequestCapability("s", "PUT", "/a", 1_000_000)).not.toBe(base);
		expect(signUiRequestCapability("s", "POST", "/b", 1_000_000)).not.toBe(base);
		expect(uiCapabilityRequestSubject("post", "/a")).toBe("POST /a");
	});

	it("never contains the secret and uses the agreed header name", () => {
		expect(signUiRequestCapability("very-secret-value", "POST", "/a", 5_000)).not.toContain("very-secret-value");
		expect(UI_CAPABILITY_HEADER).toBe("X-AO-UI-Capability");
	});
});
