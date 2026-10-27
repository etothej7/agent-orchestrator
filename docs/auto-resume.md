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
2. **Idle sessions are parked, not launched.** If the harness's history probe proves the
   saved conversation exists, one write sets the sleep marker and the idle state. The
   next message wakes the session through the normal worktree-preserving path.
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
   attempts). A session that is resumed 3 times in 30 minutes is refused ("keeps stopping
   after restarts").

## Hard rules

- **Never a fresh start.** An unattended chat resume continues the saved provider
  conversation or fails; it never starts a new one and never re-runs the saved prompt.
- **`never` means protected.** `ao session set-auto-resume <id> never` (the human, or the
  main coordinator) stops auto-resume, refuses replacement and project teardown for that
  session, and makes a kill keep its worktree. Only the human can set it back to
  `allowed`. The seed protects `optimus_db-1` and `optimus_rails_app-1`. An unreadable
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
| Provider has no saved session | refused | n/a | no: one notice, never a fresh start |

Parking (idle sessions) additionally needs the adapter's history probe: claude, codex,
copilot, qwen, cline, kilocode, mimocode, omp, opencode (v1) and opencode-v2. Without a
probe an idle session is resumed instead of parked.

Terminal (TUI) sessions are not auto-resumed.

## Tests

`go test ./internal/service/autoresume ./internal/session_manager` cover the logic.
`go test ./e2e -run TestAutoResume` boots real daemons with a fake resume-only provider
and exercises kill -9, both graceful shutdown orders and a restart inside the settle
delay.
