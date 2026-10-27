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
* **The request names a staged update** (`stagedApp` + `version`). The app checks, on every poll, that
  `stagedApp` resolves (realpath, so a symlink cannot lead out) to a directory ending in `.app` inside
  `<state dir>/update/staged/<stamp>/`; that the `<stamp>` folder is a real directory (not a symlink) and the app,
  folder and `Contents/Info.plist` are owned by the user and not group- or world-writable; and that the Info.plist
  `CFBundleShortVersionString` equals `version`. Otherwise the request is rejected (`staged_invalid`) and removed.
  This is not authentication (a same-user process can fabricate a staged folder); the package's cryptographic checks
  (CDHash, hashes) stay with the runner. It stops stray, stale or half-written requests.
* The app polls once a second (packaged builds only). A valid request starts an *update quit*: no warning, no
  relaunch, and **never a dialog**. It is honoured only when ALL of these hold; otherwise the request is kept and
  the app reports `deferred`, so the quit happens as soon as they do, or the request expires:
  1. the Mac has been idle for at least 300 s (`powerMonitor.getSystemIdleTime()`) and no AO window has the focus
     (`reason: "user_active"`; a person who comes back after the quit was asked for stands it down again, and the
     relaunch net below is cancelled);
  2. no unsent chat draft is at risk (`reason: "unsent_drafts"`; never a dialog);
  3. the relaunch safety net is armed (`reason: "safety_net_unavailable"` if it cannot be: AO does not quit).
  A quit that did not take effect (the app is still here) is asked for again after 30 s. The "update quit in
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
* After **600 s** the net looks: if a cancel file exists it stops (`outcome` = `cancelled`); if an AO app process is
  running (`^<app>/Contents/MacOS/`) it stops (`app_running`); otherwise it relaunches hidden with `open -g -j <app>`
  (`relaunched`).
* It first waits while the installer is mid-swap or rolling back: `~/.ao/install-swap-marker` exists, or
  `~/.ao/install.lock/` exists with a live owner pid (or no pid yet). A stale lock (dead owner) is ignored. If that
  goes on for 45 minutes it relaunches anyway.
* **The runner cancels the net on success** by creating `relaunch-net/<stamp>/cancel` (any content). The app also
  cancels it when the quit is called off (a draft appeared, the person came back, the request expired or was
  removed), and arms it again when the quit goes ahead. Arming is idempotent per stamp.
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

## Why these choices

* A quit that never prompts is the point of the update window: the runner only starts it when the Mac is idle and no
  AO window is open, and the app still refuses to open a dialog if a draft turns up late (the quit controller holds
  the quit silently; the window `close` and `will-prevent-unload` handlers do not ask while an update quit runs).
* Naming the pid makes the file one-shot without the app having to delete anything first.
* The app never overwrites a result or a request; it only reads requests and writes its own status.
