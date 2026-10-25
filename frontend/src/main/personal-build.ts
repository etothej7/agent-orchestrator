/** Personal builds stay on their local source lineage until explicitly replaced. */
export function isPersonalBuild(version: string): boolean {
	return /^\d+\.\d+\.\d+-ed(?:\.|$)/.test(version);
}
