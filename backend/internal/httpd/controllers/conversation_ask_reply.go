package controllers

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
)

const resolveHumanDecisionPath = "/api/v1/sessions/{sessionId}/conversation/human-decisions/resolve"

// askReplyService names the coordinator question a message answers and closes it.
type askReplyService interface {
	ResolveAskReference(ctx context.Context, session domain.SessionID, ref string) (string, error)
	ResolveAskReply(ctx context.Context, askMessageID string) error
}

// askReplyTarget validates a reply's question reference before anything is sent,
// so a message can never claim to answer a question that is not there. It returns
// the question's message id ("" when the request names none) and whether to go on.
func (c *ConversationsController) askReplyTarget(w http.ResponseWriter, r *http.Request, session domain.SessionID, ref string) (string, bool) {
	if ref == "" {
		return "", true
	}
	svc, ok := c.Svc.(askReplyService)
	if !ok {
		apispec.NotImplemented(w, r, r.Method, r.URL.Path)
		return "", false
	}
	id, err := svc.ResolveAskReference(r.Context(), session, ref)
	if err != nil {
		envelope.WriteError(w, r, err)
		return "", false
	}
	return id, true
}

// finishAskReply closes the question after its reply was accepted. Closing is
// idempotent, so a client that retries a failed response with the same message id
// cannot close or publish twice.
func (c *ConversationsController) finishAskReply(w http.ResponseWriter, r *http.Request, askMessageID string) bool {
	if askMessageID == "" {
		return true
	}
	svc, ok := c.Svc.(askReplyService)
	if !ok {
		return true
	}
	if err := svc.ResolveAskReply(r.Context(), askMessageID); err != nil {
		envelope.WriteError(w, r, err)
		return false
	}
	return true
}

// resolveHumanDecision lets the coordinator record that Ed answered its question.
func (c *ConversationsController) resolveHumanDecision(w http.ResponseWriter, r *http.Request) {
	service, ok := c.Svc.(interface {
		ResolveHumanDecision(context.Context, domain.SessionID, string, string) (string, error)
	})
	if !ok {
		apispec.NotImplemented(w, r, r.Method, resolveHumanDecisionPath)
		return
	}
	var req ResolveHumanDecisionRequest
	if !decodeConversationBody(w, r, &req) {
		return
	}
	id, err := service.ResolveHumanDecision(r.Context(), domain.SessionID(chi.URLParam(r, "sessionId")), req.ID, req.Note)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, HumanDecisionResponse{ID: id})
}
