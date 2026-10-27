# Local autonomy acceptance harness

`acceptance.py` is a Python-standard-library runner for a **real one-request** AO
trial. It never creates epic/story records, assigns workers, supplies reviews,
changes workflow status, retries an agent, or nudges a coordinator between stages.
Only bootstrap configuration, one stable-key main-conversation request, and exact
predeclared answers to structured scope questions may write through the API.

The runner is for this Mac and requires `sandbox-exec`; it fails closed elsewhere.
It accepts either `--daemon /absolute/path/to/ao` or `--app /path/to/Some.app`.
`--app` selects the bundled daemon; **it never launches Electron**. There are no
installed-app, live-daemon, push, or publication operations.

## Mechanics validation (no provider credentials)

```sh
python3 test/autonomy/acceptance.py \
  --daemon /private/tmp/ao-personal-baseline-4/frontend/daemon/ao \
  --duration 15 --restart-after 4 --poll 1 \
  --output /Users/edstockman/.ao/data/artifacts/agent-orchestrator-4/story6-mechanics
python3 -m unittest discover -s test/autonomy -v
```

At baseline `c833b70`, a fresh HOME has no verified Codex account, so coordinator
spawn returns `CODEX_ACCOUNT_AUTH_UNVERIFIED` before intake. This is an expected
**NOT ACCEPTED** result, not an autonomy pass. Repository/coordinator configuration,
read-only observations, isolated daemon restart, result emission and process
cleanup can still be validated. No fixture actor or invented agent response is
substituted to bypass that boundary. A real run requires explicit provider inputs
and the future autonomy daemon.

## Real actor run

Use a daemon containing the autonomy implementation after its independent review.
The provider account must be usable in the isolated HOME. Only explicitly named auth files receive read-only symlinks into the isolated HOME; the full user profile is never linked or reused. AO may reconcile auth into its own owner-only scratch account store. The harness never reads or prints credential contents, and it deletes scratch accounts, pending-accounts and switch-staging at teardown.
Do not point a provider config at a live AO runtime or copy runtime-host sockets.

```sh
python3 test/autonomy/acceptance.py \
  --daemon /absolute/path/to/reviewed/ao \
  --mode real --agent codex \
  --agent-bin-dir /absolute/path/to/codex/bin \
  --auth-readonly /explicit/authorized/auth.json=.codex/auth.json \
  --duration 1800 --restart-after 60 --max-dispatch-latency 30 \
  --answers '{"Should punctuation be preserved?":"Yes, preserve punctuation; change spaces only."}' \
  --output /Users/edstockman/.ao/data/artifacts/agent-orchestrator-4/story6-real
```

API-key providers may instead use repeated `--pass-env OPENAI_API_KEY` (also
supports `ANTHROPIC_API_KEY` and `GEMINI_API_KEY`). No other inherited environment
is forwarded. A credential does not itself prove AO account verification; if the
provider/account cannot initialize, the run reports that failure without retrying
or sending another intake. Fix bootstrap configuration for a **new** trial.

All actor writes are confined to a newly allocated `0700` scratch root. HOME,
AO_DATA_DIR, AO_RUN_FILE, temporary files, fixture repositories and AO worktrees
are there. Actors may signal only processes in their own sandbox, never outside same-user processes. A regression test proves this against a harmless outside fixture. The runtime cannot read the invoking user's live `~/.ao`. Outbound
provider traffic is allowed only in real mode, on TLS port 443; real mode also permits the exact macOS mDNSResponder Unix socket for system DNS resolution (no arbitrary UDP access); loopback traffic permits the scratch daemon and new ephemeral provider-host listeners. Every pre-existing TCP listener port and the live AO runfile port are explicitly denied for outbound access; a socket test proves both new-host connectivity and denial to an outside fixture listener. These are loopback-only binds, never LAN listeners. Services started on new ports by unrelated processes after the inventory are outside that static denylist; do not run concurrent service-start experiments during acceptance. Provider binaries and their
dependencies must already be installed; the harness does not install them.
GOMAXPROCS=4 and GOFLAGS=-p=4 are set; fixture instructions cap active workers at
four and the observer asserts that cap. The driver itself is serial. Run any
additional Vitest checks with `--maxWorkers=4`.

Two fixture repositories contain intentionally broken functions and checked-in
failing tests. The request requires the actual developer to run the failing
checks with `python3 check.py --story-id STORY_ID` before fixing them, commit, rerun the check at the final commit, obtain a separate actor's fixed-SHA review,
and record truthful verification and Done. The harness independently executes
its original golden assertions against the reported commit's source in the
scratch sandbox; changing the fixture's test cannot fool that check. Evidence
also requires actual actor command activity with a failing check followed by a
passing check, delivered story-specific worker/reviewer turns, matching implementation/check/
review SHAs, managers and role assignments, and explicit Done. Each story needs check output tied to its repo, actor, story ID and commit; the check script at that commit must match the original. Each reviewer must execute the standalone command `git show FINAL_SHA -- feature.py` (no shell compounds, pipelines or subshells) in the matching repo after a delivered handoff. Unrelated opening turns and other stories’ passing checks cannot satisfy those gates.

In real mode, `--restart-after` is a minimum delay: the runner waits until at least two stories exist, one is unfinished, and a registered actor other than the main coordinator has a running turn. Queued turns do not qualify. Mechanics mode retains the timed restart without those autonomy prerequisites. The runner records the working actor and story IDs, kills **only its daemon** once during observation, then restarts it on
the same DB/runfile. It observes durable recovery rather than replaying intake.
Actor IDs and message IDs must survive; a completed workflow, no duplicate actors,
stable logical delivery identities and no duplicate provider turns are required.
If real work never starts before the deadline, no restart is injected and recovery acceptance fails. Do not call a mechanics restart an autonomy-recovery pass.

Report-to-next-dispatch latency uses a report's timestamp and the next durable
message correlated through that story's continuation delivery key. Unrelated
concurrent messages do not count. Every report is retained in the result: required continuation, terminal/non-actionable exclusion with reason, or missing correlation. Required missing correlations fail the evidence gate; they are never dropped from the measured set.

Public main-conversation pages and all notification pages are inspected in
memory. Only structured, human-audience scope questions/blocking decisions are
classified actionable; routine status/completion chatter is rejected. Delivery
keys, provider fields, reasoning activity/markers, and the private sentinel are
checked. Exact scope answers are sent through story commands only when one
current story owns the exact question event ID and requester, and the normalized question text matches; unmatched/ambiguous questions
remain unanswered. No tool approval or generic progress prompt is auto-answered.
Privacy scanning is an explicit sentinel/field/marker check, not a claim that a
regex can prove the absence of every conceivable secret or semantic reasoning.

## Optional seeded history check

Add `--seed-db /explicit/path/to/source.db`. The source is opened with SQLite
`mode=ro` and backed up consistently into the new scratch root. The seed runs in
a **separate confined phase**, with child execution and signals denied and no
provider network. Conversation messages, project rows and app settings are
hashed before/after; integrity, foreign keys and history API reads are checked.
The seed HOME is then parked under `seed-home`; the actor trial starts with a
fresh DB. This prevents copied active sessions from reconnecting to live actors.
A seed phase failure fails the result, never silently discards a preservation
problem. Raw snapshots are private and are not included in the output artifacts. All AO-managed scratch credential stores are removed after processes stop, with paths and absence verification recorded.

## Outputs and teardown

Each invocation emits `result.json` and `summary.md` to `--output` (default: this
worker's artifact directory). Use distinct output directories to retain trials. Results record the harness commit/source hash and daemon binary hash plus embedded VCS commit/dirty flag.
They contain IDs, binary hash, assertion results, mutation hashes, public-message
counts/classifications, actor checks, commit identities, restart/cleanup evidence
and timings; no raw transcript or provider log is attached. Private daemon logs
and retained scratch state stay under the `0700` scratch root for diagnosis.

Cleanup runs in `finally` with pre-initialized false `processes_gone` and `accounts_purged` gates; exceptions are recorded and cannot produce PASS, including setup errors, timeout, Ctrl-C and SIGTERM.
It tracks descendants and the unique inherited run marker, including detached
runtime hosts. PID start-time identity is checked before signals and before using a process as an ancestry root; historical exited PIDs cannot adopt unrelated children after PID reuse. It uses TERM,
then KILL only for survivors, and records remaining owned PIDs. Scratch data is retained except credential-store directories, which are deleted only after all owned processes have exited. The installed application and live daemon are never stopped.

Exit 0 means **all** runtime assertions passed; exit 1 means failed/not observed;
argument/initialization failure uses exit 2. Passing the harness's unit tests or
mechanics checks does not mean autonomy passed. Fixed-SHA independent review of
this harness and of the product, native app acceptance, and installation approval
are separate gates.

## Offline re-score

`python3 test/autonomy/rescore.py ORIGINAL/result.json --output ARTIFACT_DIR/offline-rescore.json` reads the retained scratch DB and Git objects only. It does not launch providers, daemons or checks, and refuses to overwrite the original result. It labels the result as the same-run re-score, reconstructs worker-only concurrency from turn lifetimes, and separates report receipt timing from recipient turn start. Neither proves the next work dispatch. Known shell argv wrappers are unwrapped once; inner proof commands must still be standalone. A failed command with a nonzero exit code is valid failure evidence, never a passing check.

## Deterministic reviewer remediation

The labels fixture deliberately has two stages. Its first implementation must fix the requested behavior while retaining `REVIEW_FIX_APPLIED=False`, so the unchanged check still fails at that commit. A separate reviewer must inspect that SHA and record `review_failed`; the manager must deliver a handoff to the same developer. Only then may the developer change the gate to true in a new commit, run the unchanged passing check, and obtain re-review at the new SHA. The observer requires linked failed and passing command evidence, both reviewer inspections, the delivered manager route, distinct ancestor/final SHAs and final review. This controlled fixture is disclosed in the original request; no driver fabricates a failure, review, message or status. All proof commands must be standalone tool invocations.

## Recipient-start latency policy

For each phase-ending report and Review/Ready/Done transition, correlate the responsible recipient through its exact report-batch message or a same-story continuation delivery. An already queued same-story turn may qualify by its actual start after the signal; an unrelated turn cannot. If idle at receipt, the bound is receipt-to-start <=30s. If busy at receipt, it is current-turn-end to correlated next start <=30s. Missing/ambiguous start/end evidence fails. Record current/target turn IDs, timestamps, intervening queue depth, raw receipt-to-start and the gap after the preceding turn. Current human questions and completed stories with no ready follow-on work are explicitly excluded, never dropped. Provider cold-start is unknown because a distinct provider-launch timestamp is not persisted; first-provider-activity latency is a separately labeled observation, not inferred cold-start. Once stories reach Done the observer continues until required timing gates settle or the overall deadline expires, so teardown does not preempt queued-turn evidence.

The remediation verifier also reads `feature.py` from both fixed Git SHAs, requires an unambiguous literal False-to-True gate change, and runs the original behavioral assertions against each revision inside the existing scratch sandbox. Initial behavior must already be fixed with the gate False; final behavior must remain correct with it True. Source hashes and stage results are recorded. The offline re-score never executes those sources and cannot award a new pass for this behavior gate.

Unclaimed phase reports need not wait for the human/report outbox: an exact `phaseReportId` continuation cursor, matching story, registered assigned manager and actual target turn establish the CDC route even when the report recipient is null. Done with eligible backlog work follows the next story in the same epic and that story’s assigned manager, not a stopped source story or a forced main-coordinator recipient. Source and dispatched story IDs are recorded separately; wrong-story/actor/cursor matches fail.

## Revised in-turn acceptance contract

After the final 66b trial, agent-orchestrator-1 authorized credit for durable,
manager-authored, same-story phase actions performed within the manager turn
already running at the trigger. The scorer requires revision-linked phase
progression: implementation to review routing, failed review to worker remediation,
passed review to Ready, or Ready to Done. An In Review trigger requires an
intervening review outcome at that revision before the next manager action.
Handoffs require a delivered new-turn receipt, the expected recipient role and
an attributable manager message containing that revision. It records trigger,
evidence/action IDs, manager/turn, revision and trigger-to-action seconds; no
bound is imposed on the manager's own turn duration. Unrelated actions and late
report batches cannot substitute for this proof. Out-of-turn bounds are unchanged.
Any retained-run re-score must disclose this post-run contract change and its
authorizer, preserve the original result, and distinguish it from a new run.

## Targeted idle-manager CDC wake run (`--scenario idle-wake`)

One real intake for a single story in a single `acceptance-idle` repo, run with the same confinement and no nudges. It measures only whether an **idle** manager is woken by story CDC (a `story-continue-*` delivery), not by the ordinary report outbox batch. It is not the full scenario and cannot yield a full acceptance.

The intake tells the manager to end its turn right after assigning the developer (a controlled condition, disclosed in the result) and the repo AGENTS.md makes the developer run a `sleep 45` first so Done lands well after the manager has idled. Scoring (`idle_wake_evidence`, IDs/timestamps only):

- Trigger: the first worker Done report of the story.
- Precondition: the assigned manager had no running turn for at least 10 s before it. If not met, the run is invalid for this question (`manager_not_idle_before_done`).
- PASS: a manager turn starts at/after the trigger within 30 s, carried by a `story-continue-*` message whose continuation phase cursor names a worker phase report of that story, and the report's outbox batch turn is not that turn.
- Recorded, not required: the manager's next durable same-story action after the wake and its latency.

The run stops as soon as the wake and a next manager action (or 45 s) are observed, or 15 s after the 30 s bound with no wake.

## ED7 auto-resume acceptance (`autoresume.py`)

Real-process acceptance for "agents come back by themselves after a restart". Each cycle gets its own confined scratch root (same sandbox, process tracking, zero-leftover teardown and credential purge as above), builds real state, simulates a restart, starts the daemon and then only **observes**: it never resumes, nudges or re-sends anything. The one post-restart write is a wake probe to a hibernated session.

State per full cycle: coordinator (idle), two idle repo managers, a third repo manager and two workers mid-turn (a long `sleep 600` command; one worker is a codex session in real runs), a session set to `autoResume=never` (stands in for the optimus pair), one stopped by the user (`exit-agent`), one terminated. Each live session's worktree gets an untracked marker file and is fingerprinted (content, modes, names, root inode and birth time, HEAD, marker) before and after.

Cycles (`--cycles`, default all): `hosts-first` (hosts and providers get SIGTERM, then the daemon), `daemon-first` (daemon SIGTERM, then hosts and providers), `kill9` (everything SIGKILL), `flap` (kill -9, then the daemon is killed twice more shortly after each start: four daemon starts, past the design's boot-storm threshold of three, so the 2-minute resumer delay applies and the cycle waits for it), `resume-failure` (one session's saved provider history is moved away inside the scratch root before the kill).

Gates, per cycle: coordinator resumed actively; working sessions resumed actively in the same provider conversation; stage barrier (coordinator, then managers, then workers) both from first-live timestamps and from the durable intent states (a later stage is never `resuming` while an earlier one is unsettled; no intent rows is no evidence); idle managers hibernated (marker set, not exited, no provider launched, measured before the wake probe) and woken by the next message through two paths (a human conversation message and an `ao send` from the coordinator), one parked manager per path; never/stopped/terminated stay down with no new turns, no intent row, policy readback `never`, stop reason `user_stop`; intent rows match (`resumed`, `hibernated`, `failed`, none for excluded); worktrees: byte-for-byte identical (content, modes, names, inode, birth) for sessions that run no agent, and for resumed ones the same directory (inode, birth), same HEAD and the dirty marker intact (a running agent legitimately writes); exactly one keyed hidden-automation "AO restarted" turn per interrupted session and none elsewhere; same provider conversation, original prompt exists at most once and (fixture runs) no `session/new` or prompt replay; no simultaneous hosts or duplicate continue turns; codex mid-turn session resumes within `--codex-settle` or leaves one clean stopped-work notice; resume failure produces exactly one stopped-work coordinator notice (matched by the display name the product writes, or the id, as whole tokens) and the other sessions still resume; no manual intervention after the restart; no panics in the daemon log. A gate that has nothing to judge fails instead of passing vacuously. Not covered: report-outbox and standing-instruction delivery to a parked manager (they need a story, which this scenario deliberately does not create).

```sh
# Mechanics: fixture ACP provider, no credentials. Never certifies (exit 1 by design).
python3 test/autonomy/autoresume.py --daemon /abs/path/to/ao --mode mechanics --output ARTIFACT_DIR
# Positive control, fixture only: resumes through the public API in stage order so the process-level gates can PASS.
# It is a manual intervention by construction, so the run still cannot pass.
python3 test/autonomy/autoresume.py --daemon /abs/path/to/ao --mode mechanics --simulate-resumer --output ARTIFACT_DIR
# Real run: same provider inputs as the one-request harness; claude-code for every role except codex_wkr.
python3 test/autonomy/autoresume.py --daemon /abs/path/to/ed7/ao --mode real \
  --auth-readonly /authorized/codex/auth.json=.codex/auth.json --pass-env ANTHROPIC_API_KEY \
  --giveup 120 --output ARTIFACT_DIR
python3 -m unittest discover -s test/autonomy -p 'test_autoresume.py' -v
```

Evidence in `result.json` is IDs, states, counts, timings and hashes only (no message text or provider data). Against a build without auto-resume the auto-resume gates fail and the safety gates pass, which is what proves the harness detects absence.
