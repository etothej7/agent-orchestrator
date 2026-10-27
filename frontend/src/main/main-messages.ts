import type { AppLocale } from "../shared/ui-locale";
import { trayCatalogs, type TrayMessageKey } from "./tray-catalog.generated";

/** Looks a main-process string up in the generated catalog, falling back to English for an unknown locale. */
export function mainMessage(locale: AppLocale, key: TrayMessageKey, vars: Record<string, string | number> = {}): string {
	const template = (trayCatalogs[locale] ?? trayCatalogs.en)[key];
	return template.replace(/\{\{(\w+)\}\}/g, (match, name: string) => String(vars[name] ?? match));
}
