# Auto-resume after a reboot

After a reboot or an AO relaunch, chat sessions that lost their agent come back on
their own, so agents never sit idle. This page is the operating summary; the code is
`backend/internal/service/autoresume` (the pass), `session_manager/resume_capture.go`
(what gets recorded at boot) and `session_manager/resume_park.go` (parking and the
history probe).

## What happens

1. **Boot records why.** The daemon stamps a `daemon_runs` row (start, heartbeat,
   shutdown, clean end). Before the boot health pass marks a session whose provider
   host is gone as `exited`, it records a *resume intent*:
   - **Rule A**: its activity was not `exited` (kill -9, power loss, a graceful quit
     where the daemon went first).
   - **Rule B**: it was `exited` and the exit was written inside the previous daemon's
     shutdown window (a graceful quit where the hosts and providers went first).
   An exit that is older, or that a person caused (`stop_reason`), is never captured.
2. **Idle sessions are parked, not launched.** Unless the harness's history probe reports
   the saved conversation is gone (one notice, nothing launched), the session is parked: one
   write sets the sleep marker and the idle state. This includes harnesses with no probe,
   because the wake of a session parked this way is *never-fresh*: it resumes the saved
   provider conversation or fails, and never starts a new one. The next message wakes it
   through the normal worktree-preserving path.
3. **Working sessions are resumed in stages**: the main coordinator, then repo and story
   managers, then workers (3 at a time, 2 s apart, 3 min stage cap, 20 s settle delay
   that grows to 2 min when the daemon keeps restarting).
4. **One continue turn.** After every stage settled, a session that was mid-turn gets one
   hidden automation turn, keyed by session and boot: "AO restarted. Continue where you
   left off. Check your last state before repeating any action". The original prompt is
   never re-sent.
5. **Failures that need you** produce one `stopped_work` notice in the coordinator
   conversation with a fixed reason ("sign-in required", "its saved conversation can't be
   resumed safely", ...). Transient failures retry with backoff (30 s to 10 min, 5
   attempts). A provider resume failure (`ErrChatResumeFailed`) or an unsettled provider
   history gets two bounded retries (3 attempts, never fresh) before the notice; sign-in,
   account, missing-binary and incompatible-driver failures are reported at once. A session that is resumed 3 times in 30 minutes is refused ("keeps stopping
   after restarts").

## Hard rules

- **Never a fresh start.** An unattended chat resume continues the saved provider
  conversation or fails; it never starts a new one and never re-runs the saved prompt.
- **`never` means protected.** `ao session set-auto-resume <id> never` (the human, or the
  main coordinator) stops auto-resume, refuses replacement and project teardown for that
  session, and makes a kill keep its worktree. Only the person can set it back to
  `allowed`, and only in the AO app: the session's `...` menu, **Allow automatic resume**, which asks for confirmation. The CLI cannot lift a protection: `ao session set-auto-resume <id> allowed` is refused by the CLI (there is no `--human` flag any more), and the daemon refuses a request to allow that the app did not confirm (403 `AUTO_RESUME_LIFT_REQUIRES_APP`). In ed.7 this is a speed bump, not enforcement: `confirmedInApp` is a plain request field, so raw HTTP can send it; it keeps the CLI, stale clients and accidents from lifting, nothing more. It is cooperative, not authentication: the daemon has no caller identity yet, so a process that deliberately forges the app's confirmation field is not stopped (the standing instructions forbid agents from posing as the human, and the caller-identity work planned for ed.8 closes this). Every change is logged with who asked and the old and new value, and setting `never` drops a pending resume intent. The seed protects `optimus_db-1` and `optimus_rails_app-1`. An unreadable
  policy reads as `never`.
- The resumer can only call a strict resume, the history probe and the park write on the
  session manager (a reflection test pins this). It cannot kill, clean up, replace or tear
  anything down.

## Which harnesses can auto-resume

Chat resume uses the provider's saved session, so what is recovered depends on the driver:

| Driver | On resume | Typed history | Auto-resume |
| --- | --- | --- | --- |
| ACP agent with `loadSession` (`claude-agent-acp` 0.70) | `session/load` | full replay imported | yes |
| ACP agent with `session/resume` only | `session/resume` | none (model context only) | yes |
| Codex app-server | `thread/resume` + `thread/read` | replay imported when the thread is settled | yes; a thread killed mid-turn may read `inProgress` until it settles (up to 45 s), then the session needs a manual resume |
| Provider has no saved session | `ErrChatResumeFailed` (an ACP "resource not found" included) | n/a | no: two bounded retries (about 30 s and 1 min, because it cannot be told from a slow start), then one notice, never a fresh start |

The adapter's history probe (claude, codex, copilot, qwen, cline, kilocode, mimocode, omp,
opencode (v1) and opencode-v2) only decides whether an idle session whose conversation is
reported gone is reported to you instead of parked. Working sessions are always resumed
(never fresh) whatever the probe says.

Terminal (TUI) sessions are not auto-resumed.

An untouched chat (a reserved provider id the provider never persisted) is not captured: there is
nothing to resume, and a first prompt that was only queued locally waits for a manual resume.
A manual resume clears a captured intent.

## Tests

`go test ./internal/service/autoresume ./internal/session_manager` cover the logic.
`go test ./e2e -run TestAutoResume` boots real daemons with a fake resume-only provider
and exercises kill -9, both graceful shutdown orders and a restart inside the settle
delay.
