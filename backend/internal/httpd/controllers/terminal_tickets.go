package controllers

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	"github.com/aoagents/agent-orchestrator/backend/internal/terminalticket"
)

// TerminalTicketIssuer mints single-use terminal tickets; *terminalticket.Store satisfies it.
type TerminalTicketIssuer interface {
	Issue() (ticket string, expiresIn time.Duration, err error)
}

// TerminalTicketsController owns POST /terminal-tickets: the app's request for permission to attach a terminal.
// Only the person's app may ask (humanOnly), and the ticket it gets works once, for seconds.
type TerminalTicketsController struct{ Tickets TerminalTicketIssuer }

// Register mounts the route on r.
func (c *TerminalTicketsController) Register(r chi.Router) {
	r.With(humanOnly).Post("/terminal-tickets", c.issue)
}

// TerminalTicketResponse carries a ticket to offer on the /mux upgrade as the subprotocol "ao.ticket.<ticket>".
type TerminalTicketResponse struct {
	Ticket           string `json:"ticket" description:"Single-use ticket; offer it on the /mux WebSocket upgrade as the subprotocol ao.ticket.<ticket>."`
	ExpiresInSeconds int    `json:"expiresInSeconds"`
}

func (c *TerminalTicketsController) issue(w http.ResponseWriter, r *http.Request) {
	if c.Tickets == nil {
		apispec.NotImplemented(w, r, "POST", "/api/v1/terminal-tickets")
		return
	}
	ticket, ttl, err := c.Tickets.Issue()
	if err != nil {
		if errors.Is(err, terminalticket.ErrTooMany) {
			envelope.WriteAPIError(w, r, http.StatusTooManyRequests, "too_many_requests", "TERMINAL_TICKETS_EXHAUSTED", "Too many terminal tickets are waiting; try again in a moment", nil)
			return
		}
		envelope.WriteError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	envelope.WriteJSON(w, http.StatusCreated, TerminalTicketResponse{Ticket: ticket, ExpiresInSeconds: int(ttl / time.Second)})
}
