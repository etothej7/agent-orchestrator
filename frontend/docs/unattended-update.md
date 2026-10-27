# Unattended update: files the update runner and the app exchange

Personal builds (`X.Y.Z-ed.N`) do not use the stock updater. A separate runner (a run-once LaunchAgent started by the
update manager) replaces the app. This page is the contract between that runner and the Electron main process. The
design is in `ed9-auto-update-design.md` (items 4, 6 and 7).

All files live under the AO state directory (`~/.ao`, the directory that holds `running.json`), are written
atomically, and are readable only by the user (files 0600, directories 0700). Nothing in them may carry a host name,
an address or a credential.

## 1. Quit request (runner -> app): `update-restart/quit-request.json`

```json
{
  "stamp": "20261027T230000Z",
  "version": "0.14.5-ed.7.2",
  "stagedApp": "/Users/me/.ao/update/staged/20261027T230000Z/Agent Orchestrator.app",
  "recoveryEntry": "/Users/me/.ao/update/staged/20261027T230000Z/recover.sh",
  "appPid": 4242,
  "runnerPid": 9001,
  "issuedAt": 1800000000000,
  "expiresAt": 1800000300000
}
```

* `appPid` is the pid of the app that must quit. **Only that process acts on the file.** A relaunched app has another
  pid, so a request left behind by an earlier run can never quit the next app: it rejects the file and removes it.
* The file must be a regular file owned by the user, mode 0600 (no group or world access), at most 4 KB, not a symlink.
* `expiresAt - issuedAt` is at most 30 minutes; `issuedAt` may be at most 60 s in the future.
* `recoveryEntry` is optional: the installer's recovery entry (an absolute path), which the relaunch net runs if an
  install is left half-swapped (see 2b). The net only runs it if it is a regular file owned by the user, executable and
  not group- or world-writable.
* **The request names a staged update** (`stagedApp` + `version`). The app checks, on every poll, that
  `stagedApp` resolves (realpath, so a symlink cannot lead out) to a directory ending in `.app` inside
  `<state dir>/update/staged/<stamp>/`; that the `<stamp>` folder is a real directory (not a symlink) and the app,
  folder and `Contents/Info.plist` are owned by the user and not group- or world-writable; and that the Info.plist
  `CFBundleShortVersionString` equals `version`. The Info.plist is resolved with realpath too and must still be inside the
  staged app, so a symlinked `Contents/` or Info.plist cannot lead out. Otherwise the request is rejected (`staged_invalid`) and removed.
  This is not authentication (a same-user process can fabricate a staged folder); the package's cryptographic checks
  (CDHash, hashes) stay with the runner. It stops stray, stale or half-written requests.
* The app polls once a second (packaged builds only). A valid request starts an *update quit*: no warning, no
  relaunch, and **never a dialog**. It is honoured only when ALL of these hold; otherwise the request is kept and
  the app reports `deferred`, so the quit happens as soon as they do, or the request expires:
  1. the Mac has been idle for at least 300 s (`powerMonitor.getSystemIdleTime()`) and no AO window has the focus.
     AO's shell is a `BaseWindow`, so focus is read from `BaseWindow.getFocusedWindow()` (and `BrowserWindow`'s)
     (`reason: "user_active"`);
  2. no unsent chat draft is at risk (`reason: "unsent_drafts"`; never a dialog);
  3. the relaunch safety net is armed (`reason: "safety_net_unavailable"` if it cannot be: AO does not quit).
  **Once the quit has been asked for, it is under way**: for the next 30 s the gates are not asked again and nothing
  is cancelled (the app can take seconds to tear down, and nothing can stop the quit any more; the relaunch net stays
  armed). Only a quit that did not take effect (a draft held it in `before-quit`, or the app is still here after the
  30 s) is stood down: the gates apply again, the net is cancelled if a gate defers, and the quit is asked for again
  when they pass. The "update quit in
  progress" state that suppresses the draft dialog lasts at most 20 s of quiet, and is cleared when the watcher
  stands the quit down.

## 2. Quit status (app -> runner): `update-restart/quit-status.json`

```json
{ "state": "quitting", "stamp": "20261027T230000Z", "pid": 4242, "at": 1800000001000 }
```

`state` is `quitting`, `deferred` (`reason`: `unsent_drafts`, `user_active` or `safety_net_unavailable`), `rejected` or
`expired`. For `rejected`, `reason` is one of `invalid`, `not_yet_valid`, `lifetime`, `unsafe_file`, `wrong_target`,
`staged_invalid`. Reasons are fixed words, never raw errors.
The runner treats a missing status as "not seen yet" and relies on the app and daemon actually exiting.

## 2b. Relaunch safety net (app -> detached process; the runner cancels it): `update-restart/relaunch-net/<stamp>/`

AO must never stay closed, whatever dies. Before the app honours a quit it arms a detached process in its own session
(it outlives the app) that does not depend on the runner:

* `net.pid` is written by the net as soon as it runs; the app only quits once it exists.
* After **600 s** the net looks (a net pid whose process is not a relaunch net, for example a reused pid number, is
  not trusted: arming starts a real one): if a cancel file exists it stops (`outcome` = `cancelled`); if an AO app process is
  running (`^<app>/Contents/MacOS/`) it stops (`app_running`); otherwise it relaunches hidden with `open -g -j <app>`
  (`relaunched`).
* It waits only for a **live installer**: `~/.ao/install.lock/` exists with a live owner pid (or no pid yet; the installer holds
  its lock for its whole run and removes it last). A stale lock (dead owner) is ignored. If a live installer goes on for 45
  minutes it gives up waiting. The lock's pid must be digits only; a sign, a space or text is a stale lock. The `pgrep`
  pattern is the app path with regex characters escaped, and `pgrep` is run with `-a` (macOS `pgrep` otherwise skips the
  caller's ancestors, which includes the app that spawned the net).
* **A swap marker (`~/.ao/install-swap-marker`) with no live installer, or still there when the wait for a live installer runs
  out, means the bundle may be half-swapped, and the net NEVER relaunches** (launching it would block the installer's own
  startup recovery, which refuses while the app or daemon runs with a marker). A marker with no live lock owner means the
  installer died mid-swap, so recovery starts as soon as the 600 s delay ends rather than after a long wait. The net runs the
  installer's recovery entry if one is recorded (the request's `recoveryEntry`, or a line `M_RECOVERY="<absolute path>"` in the
  marker), only if it is a regular, executable file owned by the user and not group- or world-writable (checked in main for
  the request's entry and again in the net for either). After a recovery that exits 0 the net **checks** that AO is running (up
  to 120 s): running -> `outcome` = `recovery_ok`; not running and the marker gone -> the net relaunches hidden itself
  (`recovery_relaunched`); not running and the marker still there -> `recovery_incomplete`, AO stays closed. A recovery that
  fails -> `recovery_failed`; no usable recovery entry -> `blocked_marker`. In every closed case the net writes a stopped
  result (`update/runs/<stamp>/result.json`, write-once, a runner's result is never overwritten) with one failed check,
  `relaunch_net_blocked_marker`, `relaunch_net_recovery_failed` or `relaunch_net_recovery_incomplete`, and a plain text ("AO
  was left closed on purpose: an update was interrupted part-way and the installer has to recover it before AO can start.
  Nothing was relaunched."), so the update manager's single message tells the person. The file passes `validateUpdateResult`.
* **The runner cancels the net on success** by creating `relaunch-net/<stamp>/cancel` (any content). The app also
  cancels it when the quit is called off (a draft appeared, the person came back, the request expired or was
  removed), and arms it again when the quit goes ahead. Arming is idempotent per stamp.
* **Runner contract:** cancel the net only after the new app has been verified running (the installer's post-launch
  checks), not when the swap finishes. If `validateUpdateResult` refuses a result (for example a long path-like string
  with digits trips the base64 rule, or `abc::def` looks like IPv6), write a generic fallback text rather than losing
  the failure message.
* The net runs with a minimal environment (no inherited secrets) and writes `outcome` once, then exits.

## 3. Result (runner -> update manager): `update/runs/<stamp>/result.json`

```json
{
  "schemaVersion": 1,
  "stamp": "20261027T230000Z",
  "outcome": "ok",
  "fromVersion": "0.14.5-ed.7.1",
  "toVersion": "0.14.5-ed.7.2",
  "text": "AO updated to 0.14.5-ed.7.2. Your agents kept running.",
  "checks": [{ "name": "daemon", "result": "ok" }],
  "at": 1800000600000
}
```

* `outcome`: `ok`, `rolled_back`, `stopped`, `deferred`. `checks[].result`: `ok`, `fail`, `skip`.
* The file is **write-once**: the same content again is accepted (a retry), different content for the same stamp is
  refused. `main/update-result.ts` validates, refuses sensitive text and writes it. Text and check names are
  normalized first (NFKC, so look-alike and full-width forms fold; invisible format characters such as zero-width
  space, word joiner, soft hyphen and BOM, and control characters, are removed) and the sanitized form is what is
  scanned and stored. Refused: URLs, IPv4 and IPv6 addresses, `.ts.net`, `trycloudflare` and `.local` names, `Bearer`
  tokens and `password=`/`token:`/`api_key=`-style secrets, `sk-`, `ghp_`-style, JWT, AWS (`AKIA`/`ASIA`) and Slack
  (`xox…`) tokens, 32+ hex characters, and long (40+) base64 runs that mix letters and digits.
* The update manager reads it and tells the coordinator, which sends the single `ao tell` for the update.

## 4. The quiet window: the daemon defers wakes and deliveries (SPEC for ed.8 / ed.9, not in ed.7.2)

Lesson from the ed.7 install: a chat host that restarts while the quit is pending changes the host set, and the
installer's own "host set changed" check then stops the install. The window between "the runner decides to update" and
"the new app is verified" must therefore be quiet: no session starts, restarts or is woken, and nobody is asked
anything. ed.7.2 does not have this (its gates are idle Mac, no focused window, no unsent draft, staged package, armed
net); until the daemon side exists the **runner must not write the request unless every session is idle and the host
set has been stable** (the installer's check stays as the backstop).

**Pending request.** `update-restart/quit-request.json` exists and passes the same checks main applies: a regular
file owned by the user, mode 0600, at most 4 KB, valid JSON, `expiresAt` in the future and at most 30 minutes after
`issuedAt`. The daemon re-checks this at most once a second while any session is idle or a wake is queued; it never
trusts a file that fails them, and it never deletes the file (main and the runner own it).

**While a request is pending the daemon:**
1. does not start or restart any chat host, and does not auto-resume or resume any session (ed.7 auto-resume
   included);
2. does not deliver a wake or a message that would start a turn in an idle session, and does not start a manager turn
   for a CDC event or a settled report batch: each is queued durably, in order, with its original time;
3. lets turns that are already running finish normally (they are not interrupted, killed or steered);
4. spawns nothing new (no worker, reviewer, scratch daemon or notification helper), and sends no notification that asks
   the person to do something;
5. answers `GET /api/v1/update/quiet` with `{ "quiet": bool, "busySessions": n, "queued": n, "hostSetStableSeconds": s,
   "frozen": bool }` and writes the same to `update-restart/freeze-status.json` (0600, atomic).

**The quiet gate for the quit.** From ed.8/ed.9 main also asks the daemon before it honours a request (after the staged,
idle and draft gates and before the net is armed): it proceeds only when `busySessions == 0` and
`hostSetStableSeconds >= 30`; otherwise it defers with `reason: "sessions_busy"` and keeps the request alive like a draft
deferral. The runner reads the same endpoint before it writes the request.

**Release.** The freeze lifts when the request file is gone (success, cancel or expiry), when `expiresAt` plus 60 s
has passed (a stale file can never freeze AO), or when the daemon stops. On release the queued wakes and deliveries
run in their original order, each once. If the update succeeded, the new daemon starts with the queue it inherits
from the durable store, so nothing is lost across the swap.

**Never ask the person during the quit window.** No dialog, no sign-in prompt, no permission request, no notification
that needs an answer: anything that would need the person is deferred until the window is over, and then reported in
the single update message. The same rule applies to the runner and to this build's quit path (it never opens a
dialog; a draft defers the quit).

**Tests the daemon change must carry:** a pending valid request freezes wakes, deliveries, host starts and auto-resume;
running turns finish; an expired, wrong-mode, oversized or unreadable request freezes nothing; the freeze lifts when
the file disappears and at expiry plus 60 s, and queued items then run once, in order; `/update/quiet` reports busy
sessions and host-set stability; a host set that changed within 30 s refuses the quit gate.

## Why these choices

* A quit that never prompts is the point of the update window: the runner only starts it when the Mac is idle and no
  AO window is open, and the app still refuses to open a dialog if a draft turns up late (the quit controller holds
  the quit silently; the window `close` and `will-prevent-unload` handlers do not ask while an update quit runs).
* Naming the pid makes the file one-shot without the app having to delete anything first.
* The app never overwrites a result or a request; it only reads requests and writes its own status.
