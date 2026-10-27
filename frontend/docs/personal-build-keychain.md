# Personal builds and the macOS keychain

Personal builds (`X.Y.Z-ed.N`, ad-hoc signed) run Chromium with its **stand-in key** instead of the macOS login
keychain (`main/keychain-policy.ts`, applied at `main.ts` before the app is ready). Decision: Ed, 2026-10-27.

## What this means
* The app never contacts the keychain, so updates do not ask for the Mac password. Before this, every new ad-hoc build
  was a new code identity to the "Orchestrator.inc Safe Storage" item and asked twice on its first launch.
* **The existing "Orchestrator.inc Safe Storage" item is left exactly as it is**: no code, test or installer reads,
  changes or deletes it (`keychain-policy.test.ts` checks the source for that).
* Trade-off: logins saved in AO's Browser panel from now on are protected by the file permissions of the AO folder
  (`~/.ao/electron` is mode 0700, readable only by the Mac user), not by the keychain. They are not strongly
  encrypted, so programs running as the same Mac user could read them. This applies to **cookies in persistent or
  imported Browser profiles** (named and imported profiles persist across restarts, so their cookies are protected by
  file permissions only). Temporary Browser partitions are discarded when AO quits. Nothing sensitive is stored in
  that place today (no `cloud-auth.bin`, and no encrypted data in the existing profile).
* **That stays true on purpose:** while the stand-in key is in use in a packaged build, the cloud sign-in
  (`main/cloud-auth.ts`, which would otherwise write `cloud-auth.bin` with Electron's safeStorage) refuses to persist
  anything. The sign-in lives in memory for that run only, so after an AO restart the person signs in again.
  `safeStorageUsesStandInKey` in `keychain-policy.ts` decides this; unpackaged dev runs are unchanged. Switching back
  to the real keychain (below) makes cloud sign-in persist again, in the same change.
* Not affected: the agent tools' own sign-ins (for example Claude Code's), which they read from the keychain
  themselves, and the daemon's read of Claude Code's login through the macOS `security` tool. Those did not prompt
  before and do not now.

## Switching back
1. In `frontend/src/main/keychain-policy.ts`, change `shouldUseMockKeychain` so it returns `false` for packaged
   personal builds (keep `!input.isPackaged` if dev runs should still avoid the keychain). Update the matrix in
   `keychain-policy.test.ts`.
2. The old item still exists and belongs to the stock team's signature, so **the first launch of a real-keychain build
   asks for the Mac password twice** (the item's access list, then its partition list). Choosing "Always Allow" only
   binds to that one build's code hash: **every later ad-hoc build would ask twice again**, as it did before.
3. **Browser-panel logins saved under the stand-in key are lost** on switching back: the real key cannot decrypt
   them, so Chromium drops them and each site needs signing in again.
4. Nothing else changes; no migration is needed in either direction.
