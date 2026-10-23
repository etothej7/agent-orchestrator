# ao story

Record an explicit story decision or assignment through the daemon:

```text
ao story --story <id> --id <stable-operation-key> --kind <kind> --text <public-explanation>
```

Use `--session <id> --role manager|worker|reviewer` for `assignment`.
Assignments and `message`/`handoff` actually deliver to the selected Chat agent.
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
