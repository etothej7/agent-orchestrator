import { describe, expect, it } from "vitest";
import { APP_LOCALES } from "../shared/ui-locale";
import { mainMessage } from "./main-messages";
import { TRAY_MESSAGE_KEYS } from "./tray-catalog.generated";

describe("mainMessage", () => {
	it("fills {{vars}} and leaves an unknown placeholder visible", () => {
		expect(mainMessage("en", "menu.quit", { appName: "AO" })).toBe("Quit AO");
		expect(mainMessage("en", "menu.quit")).toBe("Quit {{appName}}");
	});

	it("falls back to English for a locale the catalog does not have", () => {
		expect(mainMessage("xx" as never, "quit.keepRunning")).toBe("Keep Running");
	});

	it.each(APP_LOCALES)("has every main-process key, non-empty, for %s", (locale) => {
		for (const key of TRAY_MESSAGE_KEYS) expect(mainMessage(locale, key).trim().length, `${locale} ${key}`).toBeGreaterThan(0);
		expect(mainMessage(locale, "menu.quit")).toContain("{{appName}}");
	});
});
