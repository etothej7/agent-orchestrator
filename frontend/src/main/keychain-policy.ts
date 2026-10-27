import { isPersonalBuild } from "./personal-build";

/**
 * Whether Chromium uses its stand-in key instead of the macOS login keychain.
 *
 * Why: Chromium keeps its cookie and safeStorage key in the keychain item "<app name> Safe Storage". macOS
 * binds access to that item to the app's code identity. Personal builds are ad-hoc signed, so every build is a
 * new identity and the first launch of each asks for the Mac password twice, which defeats an unattended
 * update. With the stand-in key the app never contacts the keychain: nothing is read, written or asked, and the
 * existing item is left exactly as it is.
 *
 * Trade-off (Ed's decision, 2026-10-27): logins saved in the Browser panel afterwards are protected by the file
 * permissions of the AO folder (readable only by the Mac user), not by the keychain. The cloud auth store is
 * empty today (no cloud-auth.bin), and every Browser-panel partition is temporary.
 *
 * To switch back, change this function (return false for personal builds). See
 * frontend/docs/personal-build-keychain.md for what that means for the old item and for saved logins.
 */
export function shouldUseMockKeychain(input: { isPackaged: boolean; version: string; platform: NodeJS.Platform }): boolean {
	if (input.platform !== "darwin") return false;
	// An unsigned dev run is a new app to macOS whenever its binary changes; a packaged personal build is too.
	return !input.isPackaged || isPersonalBuild(input.version);
}
