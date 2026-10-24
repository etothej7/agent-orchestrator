// Package story joins explicit coordination exchanges without owning session lifecycle.
package story

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
)

// Store is the durable story boundary.
type Store interface {
	StoryReplies(context.Context, string) ([]domain.StoryReplyThread, error)
	TurnByID(context.Context, string) (domain.ConversationTurn, error)
	GetReport(context.Context, string) (domain.ReportRecord, bool, error)
	ListEpics(context.Context) ([]domain.Epic, error)
	CreateEpic(context.Context, domain.Epic) error
	ListStories(context.Context) ([]domain.Story, error)
	GetStory(context.Context, string) (domain.Story, error)
	CreateStory(context.Context, domain.Story) error
	StoryEvents(context.Context, string) ([]domain.StoryEvent, error)
	ApplyStoryEvent(context.Context, domain.Story, domain.StoryEvent, string) (domain.StoryEvent, bool, error)
	FinishStoryDelivery(context.Context, domain.StoryEvent) error
	GetSession(context.Context, domain.SessionID) (domain.SessionRecord, bool, error)
	GetProject(context.Context, string) (domain.ProjectRecord, bool, error)
	GetCoordination(context.Context) (domain.CoordinationConfig, error)
}

// Messenger reuses Chat's durable steer-or-send receipt and sender attribution.
type Messenger interface {
	SteerOrSend(context.Context, domain.SessionID, ports.ChatUserMessage, bool) (chatsvc.SteerOrSendResult, error)
}

// ActionRecorder links confirmed decisions into the existing agent chat.
type ActionRecorder interface {
	RecordStoryAction(context.Context, domain.SessionID, domain.StoryEvent)
}

// Service exposes the shared workspace.
type Service struct {
	Actions   ActionRecorder
	Store     Store
	Messenger Messenger
}

// Command is an explicit, retryable story operation. Revision prevents lost updates.
type Command struct {
	Outcome            string           `json:"outcome"`
	ID                 string           `json:"id"`
	StoryID            string           `json:"storyId"`
	Revision           int64            `json:"revision"`
	Kind               string           `json:"kind"`
	SenderSessionID    domain.SessionID `json:"senderSessionId"`
	RecipientSessionID domain.SessionID `json:"recipientSessionId"`
	Text               string           `json:"text"`
	Reference          string           `json:"reference"`
	Role               string           `json:"role"`
}

func invalid(message string) error { return apierr.Invalid("INVALID_STORY", message, nil) }

// Create validates links against real projects and coordinator configuration.
func (s *Service) Create(ctx context.Context, story domain.Story) error {
	if !validID(story.ID) || !validID(story.EpicID) || strings.TrimSpace(story.Title) == "" || len(story.Title) > 200 || len(story.Scope) > 10000 {
		return invalid("Story id, epic and title are required")
	}
	cfg, err := s.Store.GetCoordination(ctx)
	if err != nil {
		return err
	}
	if cfg.CoordinatorProjectID == "" {
		return invalid("Configure an overall coordinator first")
	}
	if len(story.ProjectIDs) == 0 || len(story.ProjectIDs) > 100 {
		return invalid("Choose the repositories in scope")
	}
	seen := map[domain.ProjectID]bool{}
	for _, id := range story.ProjectIDs {
		if seen[id] || !cfg.Manages(id) {
			return invalid("Choose distinct managed repositories")
		}
		seen[id] = true
		p, ok, err := s.Store.GetProject(ctx, string(id))
		if err != nil {
			return err
		}
		if !ok || !p.ArchivedAt.IsZero() {
			return invalid("Project is unavailable")
		}
	}
	story.Status = "backlog"
	story.Revision = 1
	story.Assignments = []domain.StoryAssignment{}
	story.BlockedReason = ""
	story.Question = ""
	story.QuestionEventID = ""
	story.QuestionRequesterSessionID = ""
	return s.Store.CreateStory(ctx, story)
}

// CreateEpic creates a named grouping; identifiers are caller-owned retry handles.
func (s *Service) CreateEpic(ctx context.Context, e domain.Epic) error {
	if !validID(e.ID) || strings.TrimSpace(e.Title) == "" || len(e.Title) > 200 {
		return invalid("Epic id and title are required")
	}
	return s.Store.CreateEpic(ctx, e)
}
func validID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, c := range id {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// Apply records explicit decisions before delivery. A retry always retains its original payload.
func (s *Service) Apply(ctx context.Context, c Command) (domain.StoryEvent, error) {
	if c.Kind == "answer" {
		c.RecipientSessionID = ""
	}
	if c.Kind != "assignment" {
		c.Role = ""
	}
	if c.Kind != "testing" {
		c.Outcome = ""
	}
	if c.Kind == "assignment" || c.Kind == "message" || c.Kind == "handoff" || c.Kind == "answer" {
		c.Reference = ""
	}

	if !validID(c.ID) || !validID(c.StoryID) || len(c.Text) > 10000 || len(c.Reference) > 2000 {
		return domain.StoryEvent{}, invalid("Valid command and story ids are required")
	}
	story, err := s.Read(ctx, c.StoryID)
	if err != nil {
		return domain.StoryEvent{}, err
	}
	events, err := s.Store.StoryEvents(ctx, c.StoryID)
	if err != nil {
		return domain.StoryEvent{}, err
	}
	// Check a retry before applying current-state guards, which may have changed.
	for _, e := range events {
		if e.ID == c.ID {
			return s.retry(ctx, c, e)
		}
	}
	if story.Revision != c.Revision {
		return domain.StoryEvent{}, apierr.Conflict("STORY_CHANGED", "Story changed; reload before making this decision", nil)
	}
	if c.SenderSessionID != "" && !assigned(story, c.SenderSessionID, "") && !s.coordinator(ctx, c.SenderSessionID) {
		return domain.StoryEvent{}, invalid("Sender must be assigned to this story")
	}
	e := domain.StoryEvent{ID: c.ID, StoryID: c.StoryID, Kind: c.Kind, SenderSessionID: c.SenderSessionID, RecipientSessionID: c.RecipientSessionID, Text: c.Text, Reference: c.Reference, Result: "recorded", CreatedAt: time.Now().UTC()}
	switch c.Kind {
	case "assignment":
		if c.Role != "manager" && c.Role != "worker" && c.Role != "reviewer" {
			return e, invalid("Choose manager, worker, or reviewer")
		}
		session, ok, err := s.Store.GetSession(ctx, c.RecipientSessionID)
		if err != nil {
			return e, err
		}
		if !ok || session.IsTerminated {
			return e, invalid("Choose an active session")
		}
		linked := false
		for _, id := range story.ProjectIDs {
			linked = linked || id == session.ProjectID
		}
		if !linked {
			return e, invalid("Session must belong to a story repository")
		}
		for _, a := range story.Assignments {
			if a.SessionID == c.RecipientSessionID {
				return e, invalid("Session is already assigned; use a separate reviewer session")
			}
		}
		story.Assignments = append(story.Assignments, domain.StoryAssignment{SessionID: c.RecipientSessionID, Role: c.Role})
		e.Result = "pending"
	case "message", "handoff":
		if !assigned(story, c.RecipientSessionID, "") {
			return e, invalid("Recipient must be assigned to this story")
		}
		e.Result = "pending"
	case "blocked":
		if strings.TrimSpace(c.Text) == "" {
			return e, invalid("Explain the blocker")
		}
		story.BlockedReason = c.Text
	case "unblocked":
		if strings.TrimSpace(c.Text) == "" {
			return e, invalid("Explain how the blocker was resolved")
		}
		story.BlockedReason = ""
	case "question":
		if strings.TrimSpace(c.Text) == "" {
			return e, invalid("Question is required")
		}
		if c.SenderSessionID == "" {
			return e, invalid("Question must identify its assigned requester")
		}
		story.Question = c.Text
		story.QuestionEventID = c.ID
		story.QuestionRequesterSessionID = c.SenderSessionID
	case "answer":
		if strings.TrimSpace(c.Text) == "" || story.Question == "" {
			return e, invalid("Answer an unresolved question")
		}
		if story.QuestionRequesterSessionID == "" {
			return e, invalid("Question has no recorded requester; record a new attributed question")
		}
		e.RecipientSessionID = story.QuestionRequesterSessionID
		e.RelatedEventID = story.QuestionEventID
		e.Result = "pending"
	case "implementation", "testing", "installation", "release":
		if c.Kind == "implementation" && !assigned(story, c.SenderSessionID, "worker") {
			return e, invalid("Implementation evidence must be recorded by its assigned worker")
		}
		if c.Kind == "testing" {
			if c.Outcome != "passed" && c.Outcome != "failed" {
				return e, invalid("Test evidence must say passed or failed")
			}
			e.Result = c.Outcome
		}
		if strings.TrimSpace(c.Reference) == "" || strings.TrimSpace(c.Text) == "" {
			return e, invalid("Evidence needs a result and reference")
		}
	case "review_passed", "review_failed":
		if !assigned(story, c.SenderSessionID, "reviewer") || assigned(story, c.SenderSessionID, "worker") {
			return e, invalid("Independent assigned reviewer is required")
		}
		for _, prior := range events {
			if prior.Kind == "implementation" && prior.Reference == c.Reference && prior.SenderSessionID == c.SenderSessionID {
				return e, invalid("Implementation author cannot independently review that revision")
			}
		}
		if strings.TrimSpace(c.Reference) == "" || strings.TrimSpace(c.Text) == "" {
			return e, invalid("Review requires a fixed revision or result reference and findings")
		}
	case "in_progress":
		if len(story.Assignments) == 0 {
			return e, invalid("Assign responsible agents first")
		}
		story.Status = "in_progress"
	case "in_review":
		if !has(events, "implementation") {
			return e, invalid("Record implementation evidence first")
		}
		story.Status = "in_review"
	case "ready_for_approval":
		if !ready(events) {
			return e, invalid("Implementation, passing tests and independent review for the same revision are required")
		}
		story.Status = "ready_for_approval"
	case "done":
		if story.Status != "ready_for_approval" || !ready(events) || story.BlockedReason != "" || story.Question != "" || strings.TrimSpace(c.Reference) == "" || strings.TrimSpace(c.Text) == "" {
			return e, invalid("Resolve blockers/questions and record explicit scope completion evidence after review and checks")
		}
		story.Status = "done"
	default:
		return e, invalid("Unknown story operation")
	}
	if e.Result == "pending" && strings.TrimSpace(c.Text) == "" {
		return e, invalid("Message text is required")
	}
	// Any changed verification evidence must still support the current approval.
	if (c.Kind == "implementation" || c.Kind == "testing" || c.Kind == "review_passed" || c.Kind == "review_failed") && (story.Status == "ready_for_approval" || story.Status == "done") && !ready(append(events, e)) {
		story.Status = "in_progress"
	}
	e, _, err = s.Store.ApplyStoryEvent(ctx, story, e, commandIdentity(c))
	if err != nil {
		return e, err
	}
	if e.Result == "pending" {
		return s.deliver(ctx, e, false)
	}
	if s.Actions != nil {
		s.Actions.RecordStoryAction(ctx, e.SenderSessionID, e)
	}
	return e, nil
}
func assigned(s domain.Story, id domain.SessionID, role string) bool {
	for _, a := range s.Assignments {
		if a.SessionID == id && (role == "" || a.Role == role) {
			return true
		}
	}
	return false
}
func has(es []domain.StoryEvent, kind string) bool {
	for _, e := range es {
		if e.Kind == kind {
			return true
		}
	}
	return false
}
func ready(es []domain.StoryEvent) bool {
	var impl, test, review string
	var author, reviewer domain.SessionID
	native := false
	for _, e := range es {
		switch e.Kind {
		case "implementation":
			impl = e.Reference
			author = e.SenderSessionID
			reviewer = ""
			native = false
			test = ""
			review = ""
		case "testing":
			if e.Result == "passed" {
				test = e.Reference
			} else {
				test = ""
			}
		case "review_passed":
			review = e.Reference
			reviewer = e.SenderSessionID
			native = false
		case "review_failed":
			review = ""
		case "native_review":
			if e.Result == "approved" {
				review = e.Reference
				native = e.ReviewID != "" && e.ReviewRunID != ""
			} else {
				review = ""
			}
		}
	}
	return impl != "" && author != "" && impl == test && impl == review && (native || (reviewer != "" && reviewer != author))
}
func (s *Service) retry(ctx context.Context, c Command, e domain.StoryEvent) (domain.StoryEvent, error) {
	story, err := s.Store.GetStory(ctx, c.StoryID)
	if err != nil {
		return e, err
	}
	_, _, err = s.Store.ApplyStoryEvent(ctx, story, e, commandIdentity(c))
	if err != nil {
		return e, err
	}
	if e.Result == "pending" || e.Result == "uncertain" || e.Result == "queued" {
		return s.deliver(ctx, e, false)
	}
	return e, nil
}
func (s *Service) deliver(ctx context.Context, e domain.StoryEvent, recoverOnly bool) (domain.StoryEvent, error) {
	if s.Messenger == nil {
		return e, fmt.Errorf("story messenger unavailable")
	}
	msg := ports.ChatUserMessage{Text: fmt.Sprintf("[AO story %s; exchange %s]\n%s\nReply using ao report --story %s --client-report-id <unique-stable-key> with --checkpoint/--needs-input/--stuck/--done and --note, or ao send --story with --client-message-id. Never infer a story from the current session. Report completion does not complete the story.", e.StoryID, e.ID, e.Text, e.StoryID), ClientMessageID: "story-" + e.ID, SenderSessionID: string(e.SenderSessionID), Origin: domain.MessageOriginHuman}
	result, err := s.Messenger.SteerOrSend(ctx, e.RecipientSessionID, msg, recoverOnly)
	if err != nil {
		priorResult := e.Result
		e.Result = "uncertain"
		if !errors.Is(err, chatsvc.ErrSteerDeliveryUncertain) && (errors.Is(err, chatsvc.ErrProviderRefused) || errors.Is(err, chatsvc.ErrControllerHandoff) || errors.Is(err, chatsvc.ErrSteerUnsupported) || errors.Is(err, chatsvc.ErrSteerContentUnsupported) || errors.Is(err, chatsvc.ErrTurnNotSteerable) || errors.Is(err, chatsvc.ErrSteerIdempotencyConflict) || errors.Is(err, domain.ErrClientMessageConflict) || (priorResult == "pending" && e.Reference == "" && (errors.Is(err, chatsvc.ErrNotChatMode) || errors.Is(err, chatsvc.ErrNoController) || errors.Is(err, ports.ErrSessionNotFound)))) {
			e.Result = "failed"
		}
		_ = s.Store.FinishStoryDelivery(context.WithoutCancel(ctx), e)
		return e, err
	}
	e.Result = "delivered"
	if result.Steered {
		e.Reference = result.Steer.ActivityID
		e.DeliveryKind = "steer"
	} else {
		e.Reference = result.Turn.ID
		e.DeliveryKind = "turn"
		e.Result = turnDelivery(result.Turn)
	}
	if err := s.Store.FinishStoryDelivery(ctx, e); err != nil {
		return e, err
	}
	if s.Actions != nil {
		s.Actions.RecordStoryAction(ctx, e.SenderSessionID, e)
	}
	return e, nil
}

func commandIdentity(c Command) string { c.Revision = 0; return encode(c) }

func (s *Service) coordinator(ctx context.Context, id domain.SessionID) bool {
	cfg, err := s.Store.GetCoordination(ctx)
	if err != nil {
		return false
	}
	session, ok, err := s.Store.GetSession(ctx, id)
	return err == nil && ok && !session.IsTerminated && session.Kind == domain.KindOrchestrator && session.ProjectID == cfg.CoordinatorProjectID
}

func turnDelivery(turn domain.ConversationTurn) string {
	if turn.ProviderTurnID != "" || turn.State == domain.TurnStateCompleted {
		return "delivered"
	}
	if turn.State == domain.TurnStateQueued {
		return "queued"
	}
	if turn.State == domain.TurnStateFailed || turn.State == domain.TurnStateCancelled {
		return "failed"
	}
	return "uncertain"
}

// Events projects public delivery facts from durable receipts, never provider payloads.
func (s *Service) Events(ctx context.Context, id string) ([]domain.StoryEvent, error) {
	events, err := s.Store.StoryEvents(ctx, id)
	if err != nil {
		return nil, err
	}
	for i, e := range events {
		if e.DeliveryKind != "turn" || (e.Result != "queued" && e.Result != "uncertain") || e.Reference == "" {
			continue
		}
		turn, err := s.Store.TurnByID(ctx, e.Reference)
		if err != nil {
			return nil, err
		}
		next := turnDelivery(turn)
		if next == e.Result {
			continue
		}
		e.Result = next
		if err := s.Store.FinishStoryDelivery(ctx, e); err != nil {
			return nil, err
		}
		events[i] = e
	}
	return events, nil
}

// List reconciles durable answer receipts before exposing board state.
func (s *Service) List(ctx context.Context) ([]domain.Story, error) {
	stories, err := s.Store.ListStories(ctx)
	if err != nil {
		return nil, err
	}
	for i, row := range stories {
		if row.Question == "" {
			continue
		}
		if _, err := s.Events(ctx, row.ID); err != nil {
			return nil, err
		}
		current, err := s.Store.GetStory(ctx, row.ID)
		if err != nil {
			return nil, err
		}
		stories[i] = current
	}
	return stories, nil
}

// Read settles answer receipts before a workflow command evaluates unresolved questions.
func (s *Service) Read(ctx context.Context, id string) (domain.Story, error) {
	row, err := s.Store.GetStory(ctx, id)
	if err != nil || row.Question == "" {
		return row, err
	}
	if _, err := s.Events(ctx, id); err != nil {
		return row, err
	}
	return s.Store.GetStory(ctx, id)
}
