/**
 * Switches that turn the signed app into something a same-user process can drive or inspect:
 * Chromium's remote debugging (it exposes the shell page, and with it the human capability
 * the main process signs for it) and Node's inspector. A packaged build never runs with one.
 */
const FORBIDDEN_PREFIXES = ["--remote-debugging-", "--remote-allow-origins", "--inspect", "--debug"];

/** The first forbidden switch in argv, or undefined. Compared case-insensitively, with or without `=value`. */
export function forbiddenDebugSwitch(argv: readonly string[]): string | undefined {
	for (const arg of argv.slice(1)) {
		const name = arg.split("=", 1)[0].toLowerCase();
		if (FORBIDDEN_PREFIXES.some((prefix) => name.startsWith(prefix))) return arg;
	}
	return undefined;
}
