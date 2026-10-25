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
internal. For an actual scope question use:

```bash
ao ask --id label-choice --purpose scope_question --question 'Blue or Green?' --why-blocked 'The required label is not specified.'
```

Only the overall coordinator can use this path. Other allowed purposes are
`blocking_decision` and `completion_decision`; completion alone is not a reason
to contact the human. Explain the decision and why work cannot proceed without
it. Retain the id and content on retry. Agent-resolvable technical questions go
to the responsible manager, not the human. Pending provider permission/input
controls remain available. The gate changes presentation, not stored history.
