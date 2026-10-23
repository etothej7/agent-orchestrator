package domain

import "time"

// Epic groups durable stories without changing session or PR board semantics.
type Epic struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// StoryAssignment names a participant explicitly; one session may serve many stories.
type StoryAssignment struct {
	SessionID SessionID `json:"sessionId"`
	Role      string    `json:"role"`
}

// Story stores explicit workflow decisions, never derived session activity.
type Story struct {
	ID            string            `json:"id"`
	EpicID        string            `json:"epicId"`
	Title         string            `json:"title"`
	Scope         string            `json:"scope"`
	ProjectIDs    []ProjectID       `json:"projectIds"`
	Assignments   []StoryAssignment `json:"assignments"`
	Status        string            `json:"status"`
	BlockedReason string            `json:"blockedReason"`
	Question      string            `json:"question"`
	Revision      int64             `json:"revision"`
}

// StoryEvent is an explicit public exchange or decision, not a provider transcript.
type StoryEvent struct {
	ID                 string    `json:"id"`
	StoryID            string    `json:"storyId"`
	Kind               string    `json:"kind"`
	SenderSessionID    SessionID `json:"senderSessionId"`
	RecipientSessionID SessionID `json:"recipientSessionId"`
	Text               string    `json:"text"`
	Reference          string    `json:"reference"`
	Result             string    `json:"result"`
	CreatedAt          time.Time `json:"createdAt"`
}
