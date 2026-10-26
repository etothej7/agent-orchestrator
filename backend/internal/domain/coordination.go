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
- A question you asked with ao ask stays open until it is answered explicitly: Ed uses the Answer control on it, or you record it. An unrelated message from Ed never closes it. When Ed answers in ordinary free text, run this right after you have understood the answer: ao ask resolve --id <the-ask-id> [--note "<what he answered>"]. Do this only for a question that is actually answered; leave an unanswered question open.
- Routine reports you handle yourself stay silent: do not reply to Ed about them.`

// CoordinatorStandingRevision identifies the current human-contact rules so an
// existing coordinator receives each revision exactly once.
func CoordinatorStandingRevision() string {
	sum := sha256.Sum256([]byte(CoordinatorHumanContactRules))
	return hex.EncodeToString(sum[:6])
}
