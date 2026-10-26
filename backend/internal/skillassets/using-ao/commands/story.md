# ao story

Record an explicit story decision or assignment through the daemon:

```text
ao story --story <id> --id <stable-operation-key> --kind <kind> --text <public-explanation>
```

Use `--session <id> --role manager|worker|reviewer` for `assignment`.
Assignments record responsibility. `message`/`handoff` deliver to the selected Chat agent; an assignment alone is not delivery.
Keep the operation key and payload unchanged when retrying.

Kinds: `assignment`, `message`, `handoff`, `blocked`, `unblocked`, `question`,
`answer`, `implementation`, `testing`, `review_passed`, `review_failed`,
`in_progress`, `in_review`, `ready_for_approval`, `done`, `installation`, `release`.

Evidence needs `--reference <fixed-revision-or-result-reference>`.
Testing also needs `--outcome passed|failed`. Only an explicitly assigned,
separate reviewer session can record `review_passed` or `review_failed`.
Readiness requires implementation, passed testing and independent review for
the same revision. Done additionally requires an explicit scope-completion
explanation and reference, with questions and blockers resolved. Agent idle,
turn completion, and report done never complete a story.

Story identity is explicit on each operation; a manager may serve multiple
stories concurrently. Only public messages and evidence belong here. Do not
copy private reasoning, credentials, or provider payloads into story events.

## Inspect the canonical story

`ao story list` reads the shared board. `ao story show <id>` reads public events,
responsible actors, actual delivery receipts and allowlisted replies. Never infer
completion from a report claim or a session's idle state.

## Adopt one human outcome

Only the active overall coordinator can adopt a plan. Run `ao intake context`
to read the actual human message id, then use one stable plan:

```bash
ao intake --plan '{"id":"outcome-1","sourceMessageId":"actual-message-id","epic":{"id":"epic-1","title":"Requested outcome"},"stories":[{"id":"story-1","title":"Deliverable","scope":"Agreed behavior and verification criteria","projectIds":["managed-repository"]}]}'
```

Use multiple stories for distinct deliverables and include every repository
required by each story's participants. The daemon validates the whole plan and
commits it atomically. An agent report or provider-imported prompt cannot stand
in for human authorization. Keep every id and the plan unchanged on retry;
a different plan for the same source is rejected and identifies the existing
intake/epic. Inspect and continue it instead of creating duplicate work.

Adopted stories opt into durable continuation. The daemon wakes the responsible
manager (or overall coordinator before assignment) using stable queued-message
handles. It waits for provider recovery after restart, preserves uncertain sends,
and never marks work Done. Exhausted delivery/progress budgets become a visible
blocking decision. A stopped/cancelled turn requires explicit resume; do not
work around a stop or reset an exhausted budget without the human's decision.
Manual stories remain manual.

## Human contact is actionable only

Routine coordinator output, report batches and receipt acknowledgments remain
internal. The coordinator's output is shown to the human only when it answers a
message the human typed; anything else must be addressed with `ao ask` or `ao tell`,
each of which shows a message and raises a notification. For an actual scope question use:

```bash
ao ask --id label-choice --purpose scope_question --question 'Blue or Green?' --why-blocked 'The required label is not specified.'
```

To tell the human one verified result or that authorized work has stopped (never
progress, receipts or routine reports; at most three results per 15 minutes, so
fold related results into one; stopped notices have a generous separate cap of 20
per hour, and repeating the same message under a new id is announced once):

```bash
ao tell --id label-done --kind result --message 'Labels normalize passed independent review and checks at 3f2a9c1.'
ao tell --id label-stopped --kind stopped --message 'Totals work stopped: the fixture repo is missing.' --story totals-fee
```

Retain `--id` on retry; a retry never notifies twice. `ao tell` never pauses a story.

Only the overall coordinator can use `ao ask` and `ao tell`. Include `--story <story-id>`
for adopted work: the decision and its requester-owned story pause are persisted
atomically, and normal answer delivery resolves that exact question. An unscoped
decision is rejected while autonomous work exists. Other allowed purposes are
`blocking_decision` and `completion_decision`; completion alone is not a reason
to contact the human. Explain the decision and why work cannot proceed without
it. Retain the id and content on retry. Agent-resolvable technical questions go
to the responsible manager, not the human. Pending provider permission/input
controls remain available. The gate changes presentation, not stored history.

## Stable actors and local independent review

Managers can create a worker with `ao spawn --project <repo> --mode chat --name
"Story developer" --client-request-id <stable-story-worker-key>`. Keep the key
and all options unchanged on retry. The existing daemon creation ledger returns
the same actor; it rejects changed input and incomplete startup rather than
replaying its opening prompt. Do not combine this flag with `--claim-pr`:
claim separately, so a claim failure cannot terminate a reused actor.

Use a short opening prompt to wait for assignment, assign the actor to the story,
then send a story `handoff` with scope, repository boundaries and acceptance.
The independent local reviewer is a separate actor reviewing a fixed SHA; local
review never requires publishing. Record implementation, actual testing and
review_passed/review_failed evidence at that same SHA. Findings go back to the
original developer, then the same reviewer inspects the revised SHA. Preserve
failed evidence and public result references. Ready and Done still use the
server's evidence/independence gates; none of these commands grants installation,
publication or permission to ignore required human decisions.
