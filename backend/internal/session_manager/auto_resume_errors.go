package sessionmanager

import (
	"context"
	"errors"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// ResumeFailure is how an unattended resume should treat a failed attempt.
type ResumeFailure int

const (
	// ResumeFailureNone is no failure at all (a nil error).
	ResumeFailureNone ResumeFailure = iota
	// ResumeFailureUnknown is an error nothing recognises: retried a bounded number
	// of times, then reported.
	ResumeFailureUnknown
	// ResumeFailureAlreadyRunning means the agent is already up (someone resumed it,
	// or boot reconciliation reattached it): the intent is satisfied.
	ResumeFailureAlreadyRunning
	// ResumeFailureTransient is worth retrying with backoff.
	ResumeFailureTransient
	// ResumeFailureNeedsPerson will not fix itself: sign-in, an account change, a
	// provider that cannot be reached, or a saved conversation that cannot be resumed
	// safely. Never retried and never turned into a fresh launch.
	ResumeFailureNeedsPerson
	// ResumeFailurePermanent means the session cannot be resumed at all.
	ResumeFailurePermanent
	// ResumeFailureLimited is a provider-side resume or history failure that may be
	// transient (a provider that was slow to come up, a thread that had not settled) or
	// may be a conversation that is simply gone. Retried a couple of times, never fresh,
	// then reported.
	ResumeFailureLimited
	// ResumeFailureRefused means AO must not resume it on its own (protected, or
	// stopped on purpose). Silent: a person chose this, so there is nothing to report.
	ResumeFailureRefused
)

// ResumeReason is the fixed, public phrase for a failure that needs a person. It is
// never raw error text.
type ResumeReason string

// The public reasons an unattended resume can end in.
const (
	ResumeReasonSignIn      ResumeReason = "sign-in required"
	ResumeReasonAccount     ResumeReason = "its account changed"
	ResumeReasonProvider    ResumeReason = "its provider is unavailable"
	ResumeReasonHistory     ResumeReason = "its saved conversation can't be resumed safely"
	ResumeReasonUnresumable ResumeReason = "it can't be resumed"
)

// ClassifyResumeFailure maps the error of ResumeAgentStrict (or a pre-flight check)
// to how an unattended resume should treat it. nil is not a failure.
func ClassifyResumeFailure(err error) ResumeFailure {
	switch {
	case err == nil:
		return ResumeFailureNone
	case errors.Is(err, ErrAgentNotExited):
		return ResumeFailureAlreadyRunning
	case errors.Is(err, ErrSessionProtected), errors.Is(err, ErrStoppedOnPurpose):
		return ResumeFailureRefused
	case needsPerson(err):
		return ResumeFailureNeedsPerson
	case errors.Is(err, ErrTerminated), errors.Is(err, ErrNotFound), errors.Is(err, ErrIncompleteHandle), errors.Is(err, ErrWorkspaceMissing),
		errors.Is(err, ports.ErrSessionNotFound), errors.Is(err, ports.ErrChatUnsupported):
		return ResumeFailurePermanent
	case errors.Is(err, ports.ErrChatResumeFailed), errors.Is(err, ports.ErrChatHistoryUnsettled):
		// After needsPerson: sign-in, account, binary and incompatible-driver failures
		// that also wrap ErrChatResumeFailed are never retried.
		return ResumeFailureLimited
	case errors.Is(err, ErrResumeInProgress), errors.Is(err, ErrOperationInProgress),
		errors.Is(err, ErrInterfaceTransitionInProgress),
		errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled),
		errors.Is(err, ports.ErrChatDriverUnavailable), errors.Is(err, ports.ErrChatRecoveryInconclusive),
		errors.Is(err, ports.ErrCodexCapacityProviderUnavailable), errors.Is(err, ports.ErrCodexCapacityRequestRejected):
		return ResumeFailureTransient
	default:
		return ResumeFailureUnknown
	}
}

func needsPerson(err error) bool {
	return errors.Is(err, ports.ErrChatAuthRequired) || errors.Is(err, ports.ErrAgentAccountChanged) ||
		errors.Is(err, ports.ErrAgentBinaryNotFound) || errors.Is(err, ports.ErrChatDriverIncompatible) ||
		errors.Is(err, ErrNotResumable) || errors.Is(err, ports.ErrNativeHistoryUnavailable) ||
		errors.Is(err, ports.ErrChatHistoryUnavailable) ||
		errors.Is(err, ports.ErrChatHistoryLoadFailed)
}

// ResumeFailureReason is the public phrase for a failure that needs a person. Any
// error that is not one of those reads as the generic reason.
func ResumeFailureReason(err error) ResumeReason {
	switch {
	case errors.Is(err, ports.ErrChatAuthRequired):
		return ResumeReasonSignIn
	case errors.Is(err, ports.ErrAgentAccountChanged):
		return ResumeReasonAccount
	case errors.Is(err, ports.ErrAgentBinaryNotFound), errors.Is(err, ports.ErrChatDriverIncompatible):
		return ResumeReasonProvider
	case errors.Is(err, ErrNotResumable), errors.Is(err, ports.ErrNativeHistoryUnavailable),
		errors.Is(err, ports.ErrChatHistoryUnavailable), errors.Is(err, ports.ErrChatHistoryUnsettled),
		errors.Is(err, ports.ErrChatHistoryLoadFailed), errors.Is(err, ports.ErrChatResumeFailed):
		return ResumeReasonHistory
	default:
		return ResumeReasonUnresumable
	}
}
