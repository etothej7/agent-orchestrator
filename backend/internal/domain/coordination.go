package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
)

// CoordinationConfig links one human-facing coordinator to project managers.
// These are workflow assignments, not filesystem or credential permissions.
type CoordinationConfig struct {
	CoordinatorProjectID ProjectID   `json:"coordinatorProjectId"`
	ManagedProjectIDs    []ProjectID `json:"managedProjectIds"`
}

// Manages reports whether the project has an assigned repo manager role.
func (c CoordinationConfig) Manages(id ProjectID) bool {
	return c.CoordinatorProjectID != "" && slices.Contains(c.ManagedProjectIDs, id)
}

// CoordinatorHumanContactRules is the one statement of how the overall
// coordinator reaches the human. It is the coordinator's system prompt section
// and, unchanged, the standing instruction delivered to an existing coordinator.
const CoordinatorHumanContactRules = `Reaching Ed (the human):
- Your assistant output is shown to Ed only when it answers a message Ed typed in this conversation. Output in turns started by reports, story continuations, or worker or manager messages is internal; Ed does not see it. Everything stays stored and Ed can open the internal activity view.
- To address Ed from ANY turn, use exactly one of these two commands. Each shows Ed a message and raises a notification:
  1. ao ask --id <stable-id> --purpose scope_question|blocking_decision|completion_decision --question "<specific question>" --why-blocked "<why work cannot proceed without this answer>" [--story <story-id>]: a decision or blocker only Ed can resolve. With --story it pauses that story until Ed answers.
  2. ao tell --id <stable-id> --kind result|stopped --message "<one concise message>" [--story <story-id>]: --kind result for a verified outcome (independent review and checks passed) Ed should see; --kind stopped when authorized work has stopped and cannot continue without Ed.
- ao tell is for verified results and stopped work only. Never use it for progress, receipts, routine reports or status. At most three --kind result notices are accepted per 15 minutes (a refused one says when to retry), so fold related results into one message; --kind stopped has a generous separate cap of 20 per hour. Repeating the same message under a new --id is announced once. Retain the --id on retry; a retry never notifies twice.
- Routine reports you handle yourself stay silent: do not reply to Ed about them.`

// CoordinatorDelegationRule tells the overall coordinator to end its turn after
// delegating instead of polling the repo managers.
const CoordinatorDelegationRule = `Delegating: after you delegate and the handoff is delivered, end your turn instead of polling managers. AO queues a new turn for you when a repo manager reports done, needs-input or stuck to you (a manager's checkpoint is batched for up to an hour), and when a story has no assigned manager.`

// RepoManagerEndTurnRule tells a repo manager to end its turn once the work it
// can do now is done and rely on story-event wakes. It is both a section of the
// manager's system prompt and, unchanged, the standing instruction delivered to a
// manager that is already running.
const RepoManagerEndTurnRule = `Ending your turn: once you have performed every assignment, handoff, evidence record and dispatch you can perform now, and what remains is waiting on a worker or reviewer, end your turn. Do not poll, sleep, loop or check on them, and do not hold your turn open: AO queues a new turn for you right away when a worker or reviewer on one of your stories files a done, needs-input or stuck report, or the story status changes. A worker that reports done outside a story is delivered only after a five-minute settlement window, so do not end your turn waiting on that. Keep working in the same turn for as long as there is authorized work you can do immediately, such as an unassigned role, an undelivered handoff, a finding to route or evidence to record. Do not end your turn with a handoff undelivered or while you hold a result you have not yet acted on.`

// CoordinatorStandingText is everything an existing overall coordinator is taught
// in place: how it reaches the human and how it delegates.
func CoordinatorStandingText() string {
	return CoordinatorHumanContactRules + "\n\n" + CoordinatorDelegationRule
}

func standingRevision(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:6])
}

// CoordinatorStandingRevision identifies the current coordinator rules so an
// existing coordinator receives each revision exactly once.
func CoordinatorStandingRevision() string { return standingRevision(CoordinatorStandingText()) }

// RepoManagerStandingRevision identifies the current repo-manager rule.
func RepoManagerStandingRevision() string { return standingRevision(RepoManagerEndTurnRule) }
