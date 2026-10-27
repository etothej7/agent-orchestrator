import { describe, expect, it, vi } from "vitest";
import { APP_LOCALES, type AppLocale } from "../shared/ui-locale";
import {
	buildLinuxAppMenuTemplate,
	buildMacAppMenuTemplate,
	buildWindowsAppMenuTemplate,
} from "./menu";

type MenuItem = ReturnType<typeof buildWindowsAppMenuTemplate>[number];
type SubmenuItem = NonNullable<Extract<MenuItem["submenu"], readonly unknown[]>>[number];

function viewSubmenu(template: MenuItem[] = buildWindowsAppMenuTemplate()): readonly SubmenuItem[] {
	const viewMenu = template.find((item) => item.label === "View");
	if (!viewMenu || !Array.isArray(viewMenu.submenu)) {
		throw new Error("View menu not found");
	}
	return viewMenu.submenu;
}

describe("buildWindowsAppMenuTemplate", () => {
	it("registers both plus key forms for zoom in", () => {
		const zoomInItems = viewSubmenu().filter((item) => item.role === "zoomIn");

		expect(zoomInItems).toEqual(
			expect.arrayContaining([
				expect.objectContaining({ accelerator: "Ctrl+=", role: "zoomIn" }),
				expect.objectContaining({ accelerator: "Ctrl+Plus", role: "zoomIn", visible: false }),
			]),
		);
	});

	it("keeps the direct minus accelerator for zoom out", () => {
		expect(viewSubmenu()).toContainEqual(expect.objectContaining({ accelerator: "Ctrl+-", role: "zoomOut" }));
	});
});

describe("buildLinuxAppMenuTemplate", () => {
	it("registers zoom and standard accelerators matching Linux expectations", () => {
		const zoomInItems = viewSubmenu(buildLinuxAppMenuTemplate()).filter((item) => item.role === "zoomIn");

		expect(zoomInItems).toEqual(
			expect.arrayContaining([
				expect.objectContaining({ accelerator: "Ctrl+=", role: "zoomIn" }),
				expect.objectContaining({ accelerator: "Ctrl+Plus", role: "zoomIn", visible: false }),
			]),
		);
		expect(viewSubmenu(buildLinuxAppMenuTemplate())).toContainEqual(
			expect.objectContaining({ accelerator: "Ctrl+-", role: "zoomOut" }),
		);
	});

	it("uses a guarded click handler for DevTools when provided", () => {
		const onToggleDevTools = vi.fn();
		const template = buildLinuxAppMenuTemplate(onToggleDevTools);
		const devtoolsItem = viewSubmenu(template).find(
			(item) => item.label === "Toggle DevTools",
		);

		expect(devtoolsItem).toMatchObject({
			accelerator: "Ctrl+Shift+I",
			label: "Toggle DevTools",
		});
		devtoolsItem?.click?.(undefined as never, undefined as never, undefined as never);
		expect(onToggleDevTools).toHaveBeenCalledOnce();
	});
});


describe("buildMacAppMenuTemplate", () => {
	function macViewSubmenu(onToggleDevTools = () => undefined): readonly SubmenuItem[] {
		const viewMenu = buildMacAppMenuTemplate(onToggleDevTools).find(
			(item) => item.label === "View",
		);
		if (!viewMenu || !Array.isArray(viewMenu.submenu)) {
			throw new Error("View menu not found");
		}
		return viewMenu.submenu;
	}

	it("uses a guarded click handler instead of Electron's crashing DevTools role", () => {
		const onToggleDevTools = vi.fn();
		const devtoolsItems = macViewSubmenu(onToggleDevTools).filter(
			(item) => item.label === "Toggle Developer Tools" || item.role === "toggleDevTools",
		);

		expect(devtoolsItems).toHaveLength(1);
		expect(devtoolsItems[0]).toMatchObject({
			accelerator: "Alt+Command+I",
			label: "Toggle Developer Tools",
		});
		expect(devtoolsItems[0].role).toBeUndefined();

		devtoolsItems[0].click?.(undefined as never, undefined as never, undefined as never);
		expect(onToggleDevTools).toHaveBeenCalledOnce();
	});

	it("preserves Electron's complete standard macOS menus", () => {
		const template = buildMacAppMenuTemplate(() => undefined);

		expect(template.map((item) => item.role)).toEqual(
			expect.arrayContaining(["appMenu", "fileMenu", "editMenu", "windowMenu"]),
		);
		expect(macViewSubmenu()).toContainEqual(expect.objectContaining({ role: "forceReload" }));
	});

	it("binds Close Window to Shift+Command+W so Cmd+W cannot kill the application window", () => {
		const template = buildMacAppMenuTemplate(() => undefined);
		const fileMenu = template.find((item) => item.role === "fileMenu");
		expect(fileMenu).toBeDefined();
		const submenu = fileMenu?.submenu;
		expect(Array.isArray(submenu)).toBe(true);
		expect(submenu).toContainEqual(
			expect.objectContaining({
				role: "close",
				accelerator: "Shift+Command+W",
			}),
		);
	});

});

describe("the macOS app menu's Quit", () => {
	const items = (template: ReturnType<typeof buildMacAppMenuTemplate>) =>
		(template[0]?.submenu as Array<{ role?: string; label?: string; accelerator?: string; click?: () => void }>) ?? [];

	it("is our own item (not role quit) so a user quit can be told from an AppleEvent quit", () => {
		const onQuit = vi.fn();
		const quit = items(buildMacAppMenuTemplate(vi.fn(), { appName: "Agent Orchestrator", onQuit })).find((item) => item.label?.startsWith("Quit"));
		expect(quit?.label).toBe("Quit Agent Orchestrator");
		expect(quit?.accelerator).toBe("Command+Q");
		expect(quit?.role).toBeUndefined();
		quit?.click?.();
		expect(onQuit).toHaveBeenCalledTimes(1);
	});

	it("labels the item in each UI locale, with the app name in the locale's own word order", () => {
		const labelFor = (locale: AppLocale) =>
			items(buildMacAppMenuTemplate(vi.fn(), { appName: "Agent Orchestrator", onQuit: vi.fn(), locale })).find((item) => item.accelerator === "Command+Q")?.label;
		expect(labelFor("en")).toBe("Quit Agent Orchestrator");
		expect(labelFor("de")).toBe("Agent Orchestrator beenden");
		expect(labelFor("es")).toBe("Salir de Agent Orchestrator");
		expect(labelFor("fr")).toBe("Quitter Agent Orchestrator");
		expect(labelFor("ja")).toBe("Agent Orchestratorを終了");
		expect(labelFor("ko")).toBe("Agent Orchestrator 종료");
		expect(labelFor("pt-BR")).toBe("Encerrar Agent Orchestrator");
		expect(labelFor("zh-CN")).toBe("退出Agent Orchestrator");
		for (const locale of APP_LOCALES) expect(labelFor(locale)).toContain("Agent Orchestrator");
	});

	it("has no role: quit anywhere, and keeps the standard app-menu items", () => {
		const template = buildMacAppMenuTemplate(vi.fn(), { appName: "Agent Orchestrator", onQuit: vi.fn() });
		expect(JSON.stringify(template)).not.toContain('"role":"quit"');
		expect(items(template).map((item) => item.role).filter(Boolean)).toEqual(["about", "services", "hide", "hideOthers", "unhide"]);
	});

	it("falls back to Electron's standard app menu when no quit handler is given", () => {
		expect(buildMacAppMenuTemplate(vi.fn())[0]).toEqual({ role: "appMenu" });
	});
});
