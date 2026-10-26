// Prerelease toolchains are deliberately rejected: this build requires the
// stable Go release declared in backend/go.mod or a later stable release.
export function parseGoVersion(value) {
	const match = /\bgo(\d+)\.(\d+)(?:\.(\d+))?(?![\w.])/.exec(value);
	if (!match) return null;
	return [Number(match[1]), Number(match[2]), Number(match[3] ?? 0)];
}

export function parseMinimumGoVersion(goMod) {
	const match = /^go\s+(\d+)\.(\d+)(?:\.(\d+))?\s*$/m.exec(goMod);
	if (!match) return null;
	return [Number(match[1]), Number(match[2]), Number(match[3] ?? 0)];
}

export function meetsMinimumVersion(actual, minimum) {
	for (let index = 0; index < minimum.length; index += 1) {
		if (actual[index] !== minimum[index]) return actual[index] > minimum[index];
	}
	return true;
}

// The Go release the shipped daemon is built with. backend/go.mod stays at the
// language floor that CI and the container images support; the packaged daemon
// must additionally be built with this (or a later stable) release so it carries
// its standard-library and runtime fixes.
export const SHIPPED_DAEMON_GO_VERSION = [1, 27, 3];

/** The stricter of the go.mod floor and the shipped-daemon release. */
export function requiredDaemonGoVersion(goModMinimum, shipped = SHIPPED_DAEMON_GO_VERSION) {
	return meetsMinimumVersion(goModMinimum, shipped) ? goModMinimum : shipped;
}
