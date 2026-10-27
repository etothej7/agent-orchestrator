// Package humanrequest holds the human-only actions that an agent or a terminal asked for and
// the person confirms in the app.
//
// An agent cannot do these itself (change a conversation's model, lift a protection), and a
// terminal holds no human authority, so they file a request instead. The daemon, never the
// requester, renders what the request says: the summary and the diff come from the structured
// payload plus the current state, and an approval must name the hash of exactly what was shown.
// Approving applies the stored intent with the daemon's own authority; denying or letting it
// expire tells the requester through an ordinary message.
package humanrequest

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

const (
	// RequestTTL is how long a request waits for the person.
	RequestTTL = 24 * time.Hour
	// MaxOpenPerRequester caps the open requests one requester may have, so a loop cannot bury the person.
	MaxOpenPerRequester = 5
	// MaxFiledPerMinute caps how fast one requester may file.
	MaxFiledPerMinute = 10
	// MaxOpenTotal caps open requests across every requester. Requesters are only claimed identities until
	// per-session proofs exist, so rotating a claimed id must not be a way around the per-requester caps.
	MaxOpenTotal = 25
	// approvedStaleAfter is how long a claimed approval may sit before the sweep fails it: the daemon
	// stopped between claiming the request and applying it.
	approvedStaleAfter = 2 * time.Minute
	listLimit          = 100
	applyTimeout       = 60 * time.Second
)

// Store is the persistence the service needs; *sqlite.Store satisfies it.
type Store interface {
	GetSession(ctx context.Context, id domain.SessionID) (domain.SessionRecord, bool, error)
	InsertHumanRequest(ctx context.Context, req domain.HumanRequest) (domain.HumanRequest, bool, error)
	GetHumanRequest(ctx context.Context, id string) (domain.HumanRequest, bool, error)
	FindOpenHumanRequest(ctx context.Context, kind domain.HumanRequestKind, session domain.SessionID, payloadHash string) (domain.HumanRequest, bool, error)
	CountAllPendingHumanRequests(ctx context.Context) (int, error)
	ListHumanRequests(ctx context.Context, state domain.HumanRequestState, limit int) ([]domain.HumanRequest, error)
	CountPendingHumanRequests(ctx context.Context, requester domain.HumanRequestRequester) (int, error)
	CountHumanRequestsFiledSince(ctx context.Context, requester domain.HumanRequestRequester, since time.Time) (int, error)
	ClaimHumanRequest(ctx context.Context, id string, now time.Time) (bool, error)
	DenyHumanRequest(ctx context.Context, id string, now time.Time) (bool, error)
	FinishHumanRequest(ctx context.Context, id string, state domain.HumanRequestState, result string, now time.Time) error
	ExpireHumanRequests(ctx context.Context, now, staleBefore time.Time) ([]domain.HumanRequest, error)
}

// AutoResume reads and sets a session's auto-resume policy.
type AutoResume interface {
	AutoResumePolicy(ctx context.Context, id domain.SessionID) (domain.AutoResumePolicy, error)
	SetAutoResumePolicy(ctx context.Context, id domain.SessionID, policy domain.AutoResumePolicy, sender domain.SessionID) error
}

// Chat is the conversation service's settings and provider-switch surface.
type Chat interface {
	Models(ctx context.Context, id domain.SessionID) ([]ports.ChatModel, domain.ConversationSettings, error)
	SetTurnSettings(ctx context.Context, id domain.SessionID, settings domain.ConversationSettings) (domain.ConversationSettings, error)
	SwitchProvider(ctx context.Context, id domain.SessionID, request domain.ProviderSwitchRequest) error
}

// Config wires a Service.
type Config struct {
	Store      Store
	AutoResume AutoResume
	Chat       Chat
	Messenger  ports.AgentMessenger
	Log        *slog.Logger
	Now        func() time.Time
	NewID      func() string
}

// Service files, decides and sweeps requests.
type Service struct {
	store      Store
	autoResume AutoResume
	chat       Chat
	messenger  ports.AgentMessenger
	log        *slog.Logger
	now        func() time.Time
	newID      func() string
}

// New builds a Service. Any of AutoResume, Chat and Messenger may be nil: the kinds that need
// the missing one are then refused, and a requester is simply not messaged.
func New(cfg Config) *Service {
	s := &Service{store: cfg.Store, autoResume: cfg.AutoResume, chat: cfg.Chat, messenger: cfg.Messenger, log: cfg.Log, now: cfg.Now, newID: cfg.NewID}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.newID == nil {
		s.newID = func() string { return "hr-" + uuid.NewString() }
	}
	return s
}

// FileInput is what a requester supplies. Everything the card shows is derived from it by the daemon.
type FileInput struct {
	Kind      domain.HumanRequestKind
	SessionID domain.SessionID
	Payload   json.RawMessage
	Requester domain.HumanRequestRequester
}

// File records a request for the person and returns it. Filing the same thing again while it is
// still open returns the open request.
func (s *Service) File(ctx context.Context, in FileInput) (domain.HumanRequest, error) {
	handler, ok := kinds[in.Kind]
	if !ok {
		return domain.HumanRequest{}, apierr.Invalid("HUMAN_REQUEST_KIND_UNKNOWN", fmt.Sprintf("unknown request kind %q", in.Kind), nil)
	}
	requester, err := s.requester(ctx, in.Requester)
	if err != nil {
		return domain.HumanRequest{}, err
	}
	target, found, err := s.store.GetSession(ctx, in.SessionID)
	if err != nil {
		return domain.HumanRequest{}, err
	}
	if !found {
		return domain.HumanRequest{}, apierr.NotFound("SESSION_NOT_FOUND", "no such session")
	}
	prepared, err := handler.prepare(ctx, s, target, in.Payload)
	if err != nil {
		return domain.HumanRequest{}, err
	}
	now := s.now().UTC()
	req := domain.HumanRequest{
		ID: s.newID(), Kind: in.Kind, SessionID: in.SessionID, Requester: requester,
		Payload: prepared.payload, Summary: prepared.summary, Changes: prepared.changes,
		State: domain.HumanRequestPending, CreatedAt: now, ExpiresAt: now.Add(RequestTTL),
	}
	req.PayloadHash = hashOf(req)
	if err := s.checkLimits(ctx, req, now); err != nil {
		return domain.HumanRequest{}, err
	}
	stored, inserted, err := s.store.InsertHumanRequest(ctx, req)
	if err != nil {
		return domain.HumanRequest{}, err
	}
	if inserted {
		s.log.InfoContext(ctx, "human request filed", "request", stored.ID, "kind", stored.Kind, "session", stored.SessionID, "requester", requesterLabel(stored.Requester))
	}
	return stored, nil
}

// requester accepts the requester a caller claims. A session must exist (until per-session proofs
// land the id is cooperative attribution, so the card says only what the caller said); anything else
// is a terminal with no identity.
func (s *Service) requester(ctx context.Context, in domain.HumanRequestRequester) (domain.HumanRequestRequester, error) {
	if in.Kind == domain.HumanRequesterSession && in.SessionID != "" {
		if _, found, err := s.store.GetSession(ctx, in.SessionID); err != nil {
			return domain.HumanRequestRequester{}, err
		} else if !found {
			return domain.HumanRequestRequester{}, apierr.Invalid("HUMAN_REQUEST_REQUESTER_UNKNOWN", "the requesting session does not exist", nil)
		}
		return domain.HumanRequestRequester{Kind: domain.HumanRequesterSession, SessionID: in.SessionID}, nil
	}
	return domain.HumanRequestRequester{Kind: domain.HumanRequesterTerminal}, nil
}

// checkLimits refuses a requester who already has too many open requests or files too fast. An open
// duplicate of this very request is let through: a retry must get its card back, not an error.
func (s *Service) checkLimits(ctx context.Context, req domain.HumanRequest, now time.Time) error {
	open, err := s.store.CountPendingHumanRequests(ctx, req.Requester)
	if err != nil {
		return err
	}
	recent, err := s.store.CountHumanRequestsFiledSince(ctx, req.Requester, now.Add(-time.Minute))
	if err != nil {
		return err
	}
	total, err := s.store.CountAllPendingHumanRequests(ctx)
	if err != nil {
		return err
	}
	if open < MaxOpenPerRequester && recent < MaxFiledPerMinute && total < MaxOpenTotal {
		return nil
	}
	if _, found, err := s.store.FindOpenHumanRequest(ctx, req.Kind, req.SessionID, req.PayloadHash); err != nil {
		return err
	} else if found {
		return nil
	}
	s.log.WarnContext(ctx, "human request refused: too many", "requester", requesterLabel(req.Requester), "open", open, "lastMinute", recent, "openTotal", total)
	return apierr.TooManyRequests("HUMAN_REQUEST_LIMIT", fmt.Sprintf("too many requests are already waiting for the person (%d from you, %d in all); wait for them to be decided before filing more", open, total))
}

// List returns the requests the person should see: the open ones by default, or the most recent
// of any state when all is set. It first expires whatever is overdue.
func (s *Service) List(ctx context.Context, all bool) ([]domain.HumanRequest, error) {
	s.Sweep(ctx)
	if all {
		return s.store.ListHumanRequests(ctx, "", listLimit)
	}
	return s.store.ListHumanRequests(ctx, domain.HumanRequestPending, listLimit)
}

// Get returns one request.
func (s *Service) Get(ctx context.Context, id string) (domain.HumanRequest, error) {
	s.Sweep(ctx)
	req, found, err := s.store.GetHumanRequest(ctx, id)
	if err != nil {
		return domain.HumanRequest{}, err
	}
	if !found {
		return domain.HumanRequest{}, apierr.NotFound("HUMAN_REQUEST_NOT_FOUND", "no such request")
	}
	return req, nil
}

// Approve applies a pending request. payloadHash must be the hash of the request the person was
// shown; anything else is refused. Approving an already applied request with the right hash is a
// no-op that returns it, so a retried click is harmless.
func (s *Service) Approve(ctx context.Context, id, payloadHash string) (domain.HumanRequest, error) {
	req, err := s.Get(ctx, id)
	if err != nil {
		return domain.HumanRequest{}, err
	}
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(payloadHash)), []byte(req.PayloadHash)) != 1 {
		return domain.HumanRequest{}, apierr.Conflict("HUMAN_REQUEST_CHANGED", "this request is not the one that was shown; reload and review it again", nil)
	}
	switch req.State {
	case domain.HumanRequestApplied:
		return req, nil
	case domain.HumanRequestPending:
	case domain.HumanRequestExpired:
		return domain.HumanRequest{}, apierr.Conflict("HUMAN_REQUEST_EXPIRED", "this request expired before it was approved", nil)
	default:
		return domain.HumanRequest{}, apierr.Conflict("HUMAN_REQUEST_DECIDED", fmt.Sprintf("this request is already %s", req.State), nil)
	}
	claimed, err := s.store.ClaimHumanRequest(ctx, id, s.now())
	if err != nil {
		return domain.HumanRequest{}, err
	}
	if !claimed {
		return domain.HumanRequest{}, apierr.Conflict("HUMAN_REQUEST_DECIDED", "this request was just decided; reload", nil)
	}
	// The action finishes even if the app goes away mid-request.
	applyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), applyTimeout)
	defer cancel()
	state, result, applyErr := s.apply(applyCtx, req)
	finishErr := s.store.FinishHumanRequest(applyCtx, id, state, result, s.now())
	if finishErr != nil {
		s.log.ErrorContext(ctx, "human request outcome not recorded", "request", id, "error", finishErr)
	}
	s.log.InfoContext(ctx, "human request decided", "request", id, "kind", req.Kind, "session", req.SessionID, "decision", "approved", "outcome", state)
	finished, _, _ := s.store.GetHumanRequest(ctx, id)
	if finished.ID == "" {
		finished = req
		finished.State, finished.Result = state, result
	}
	s.tell(ctx, finished)
	if applyErr != nil {
		return finished, applyErr
	}
	return finished, nil
}

func (s *Service) apply(ctx context.Context, req domain.HumanRequest) (domain.HumanRequestState, string, error) {
	handler, ok := kinds[req.Kind]
	if !ok {
		return domain.HumanRequestFailed, "unknown request kind", apierr.Conflict("HUMAN_REQUEST_FAILED", "unknown request kind", nil)
	}
	target, found, err := s.store.GetSession(ctx, req.SessionID)
	if err != nil {
		return domain.HumanRequestFailed, "could not read the session", err
	}
	if !found {
		return domain.HumanRequestFailed, "the session no longer exists", apierr.Conflict("HUMAN_REQUEST_FAILED", "the session no longer exists", nil)
	}
	result, err := handler.apply(ctx, s, req, target)
	switch {
	case err == nil:
		return domain.HumanRequestApplied, result, nil
	case isStale(err):
		return domain.HumanRequestFailed, truncate(err.Error()), apierr.Conflict("HUMAN_REQUEST_STALE", truncate(err.Error()), nil)
	default:
		return domain.HumanRequestFailed, truncate(err.Error()), apierr.Conflict("HUMAN_REQUEST_FAILED", "the change could not be applied: "+truncate(err.Error()), nil)
	}
}

// Deny refuses a pending request and tells the requester. Denying one already denied is a no-op.
func (s *Service) Deny(ctx context.Context, id string) (domain.HumanRequest, error) {
	req, err := s.Get(ctx, id)
	if err != nil {
		return domain.HumanRequest{}, err
	}
	switch req.State {
	case domain.HumanRequestDenied:
		return req, nil
	case domain.HumanRequestPending:
	case domain.HumanRequestExpired:
		return domain.HumanRequest{}, apierr.Conflict("HUMAN_REQUEST_EXPIRED", "this request already expired", nil)
	default:
		return domain.HumanRequest{}, apierr.Conflict("HUMAN_REQUEST_DECIDED", fmt.Sprintf("this request is already %s", req.State), nil)
	}
	denied, err := s.store.DenyHumanRequest(ctx, id, s.now())
	if err != nil {
		return domain.HumanRequest{}, err
	}
	if !denied {
		return domain.HumanRequest{}, apierr.Conflict("HUMAN_REQUEST_DECIDED", "this request was just decided; reload", nil)
	}
	s.log.InfoContext(ctx, "human request decided", "request", id, "kind", req.Kind, "session", req.SessionID, "decision", "denied")
	req, _, _ = s.store.GetHumanRequest(ctx, id)
	s.tell(ctx, req)
	return req, nil
}

// Sweep expires overdue requests and fails approvals the daemon never finished, telling each requester.
func (s *Service) Sweep(ctx context.Context) {
	now := s.now()
	changed, err := s.store.ExpireHumanRequests(ctx, now, now.Add(-approvedStaleAfter))
	if err != nil {
		s.log.WarnContext(ctx, "human request sweep failed", "error", err)
		return
	}
	for _, req := range changed {
		s.tell(ctx, req)
	}
}

// tell writes the requester an ordinary message about how its request ended. Failing to deliver it
// never undoes the decision.
func (s *Service) tell(ctx context.Context, req domain.HumanRequest) {
	if s.messenger == nil || req.Requester.Kind != domain.HumanRequesterSession || req.Requester.SessionID == "" {
		return
	}
	var outcome string
	switch req.State {
	case domain.HumanRequestApplied:
		outcome = "was approved and applied."
	case domain.HumanRequestDenied:
		outcome = "was denied. Do not file it again unless the person asks you to."
	case domain.HumanRequestExpired:
		outcome = "expired before anyone decided."
	case domain.HumanRequestFailed:
		outcome = "was approved but could not be applied: " + req.Result
	default:
		return
	}
	message := fmt.Sprintf("[AO] Your request %s (%s) %s", req.ID, req.Summary, outcome)
	if err := s.messenger.Send(context.WithoutCancel(ctx), req.Requester.SessionID, message); err != nil {
		s.log.WarnContext(ctx, "could not tell the requester how its request ended", "request", req.ID, "requester", req.Requester.SessionID, "error", err)
	}
}

// hashOf covers everything the card shows and what approving would do: the kind, the target, the
// payload, the summary and the diff, each separated so no two different requests share an input.
func hashOf(req domain.HumanRequest) string {
	changes, _ := json.Marshal(req.Changes)
	h := sha256.New()
	for _, part := range [][]byte{[]byte(req.Kind), []byte(req.SessionID), req.Payload, []byte(req.Summary), changes} {
		_, _ = fmt.Fprintf(h, "%d:", len(part))
		_, _ = h.Write(part)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func requesterLabel(r domain.HumanRequestRequester) string {
	if r.Kind == domain.HumanRequesterSession {
		return "session " + string(r.SessionID)
	}
	return "terminal (unauthenticated)"
}

func truncate(text string) string {
	const limit = 300
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "…"
}
