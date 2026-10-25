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
