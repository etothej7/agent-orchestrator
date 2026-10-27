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
* The app polls once a second (packaged builds only). A valid request starts an *update quit*: no warning, no
  relaunch, and **never a dialog**. Unsent chat drafts defer the quit instead of asking: the request is kept and the
  quit happens as soon as no draft is at risk, or the request expires. A quit that did not take effect (the app is
  still here) is asked for again after 30 s.

## 2. Quit status (app -> runner): `update-restart/quit-status.json`

```json
{ "state": "quitting", "stamp": "20261027T230000Z", "pid": 4242, "at": 1800000001000 }
```

`state` is `quitting`, `deferred` (`reason: "unsent_drafts"`), `rejected` or `expired`. For `rejected`, `reason` is one
of `invalid`, `not_yet_valid`, `lifetime`, `unsafe_file`, `wrong_target`. Reasons are fixed words, never raw errors.
The runner treats a missing status as "not seen yet" and relies on the app and daemon actually exiting.

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
  refused. `main/update-result.ts` validates, refuses sensitive text (URLs, IPv4 addresses, `.ts.net` and
  `trycloudflare` names, token-shaped strings, 32+ hex characters) and writes it.
* The update manager reads it and tells the coordinator, which sends the single `ao tell` for the update.

## Why these choices

* A quit that never prompts is the point of the update window: the runner only starts it when the Mac is idle and no
  AO window is open, and the app still refuses to open a dialog if a draft turns up late (the quit controller holds
  the quit silently; the window `close` and `will-prevent-unload` handlers do not ask while an update quit runs).
* Naming the pid makes the file one-shot without the app having to delete anything first.
* The app never overwrites a result or a request; it only reads requests and writes its own status.
