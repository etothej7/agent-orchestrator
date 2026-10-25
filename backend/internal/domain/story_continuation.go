package domain

import "time"

// StoryContinuation is a durable delivery/checkpoint ledger, not story status.
// Revision fences concurrent dispatchers; LeaseUntil survives process restart.
// A delivery receipt is never evidence that the work has been completed.
type StoryContinuation struct {
	RecoveryID         string    `json:"recoveryId,omitempty"`
	StoryID            string    `json:"storyId"`
	Revision           int64     `json:"revision"`
	Generation         int64     `json:"generation"`
	Checkpoint         string    `json:"checkpoint"`
	State              string    `json:"state"`
	RecipientSessionID SessionID `json:"recipientSessionId"`
	DeliveryID         string    `json:"deliveryId"`
	Text               string    `json:"text"`
	TurnID             string    `json:"turnId,omitempty"`
	DeliveryAttempts   int       `json:"deliveryAttempts"`
	ProgressAttempts   int       `json:"progressAttempts"`
	LeaseUntil         time.Time `json:"leaseUntil"`
	WaitingSince       time.Time `json:"waitingSince"`
	NextAt             time.Time `json:"nextAt"`
	Reason             string    `json:"reason,omitempty"`
}

// StoryContinuationStatus is the human-facing allowlist, not provider state.
type StoryContinuationStatus struct {
	StoryID            string    `json:"storyId"`
	Revision           int64     `json:"revision"`
	State              string    `json:"state"`
	Checkpoint         string    `json:"checkpoint"`
	RecipientSessionID SessionID `json:"recipientSessionId,omitempty"`
	Reason             string    `json:"reason,omitempty"`
}
