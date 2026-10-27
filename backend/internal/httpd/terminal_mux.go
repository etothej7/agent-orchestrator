package httpd

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/requestscope"
	previewutil "github.com/aoagents/agent-orchestrator/backend/internal/preview"
	"github.com/aoagents/agent-orchestrator/backend/internal/terminal"
	"github.com/aoagents/agent-orchestrator/backend/internal/terminalticket"
)

// terminalMuxReadLimit caps a single inbound frame. Client→server frames are small
// (keystrokes, resize, control), so a generous 1 MiB is ample headroom while
// still bounding memory per message.
const terminalMuxReadLimit = 1 << 20

// mountTerminalMux registers the long-lived terminal-multiplexing WebSocket at /mux. It
// is intentionally outside the per-request Timeout middleware (the connection is
// long-lived). When mgr is nil the route is not mounted — the daemon simply has
// no terminal surface yet.
func mountTerminalMux(r chi.Router, mgr *terminal.Manager, log *slog.Logger, allowedOrigins []string, tickets *terminalticket.Store) {
	if mgr == nil {
		return
	}
	r.Get("/mux", terminalMuxHandler(mgr, log, allowedOrigins, tickets))
}

// terminalMuxHandler upgrades the request to a WebSocket and hands the connection to the
// terminal manager. httpd owns only the upgrade and the transport adaptation;
// all stream logic lives in internal/terminal.
func terminalMuxHandler(mgr *terminal.Manager, log *slog.Logger, allowedOrigins []string, tickets *terminalticket.Store) http.HandlerFunc {
	allowed := exactAllowedOrigins(allowedOrigins)
	return func(w http.ResponseWriter, r *http.Request) {
		if !terminalMuxRequestAllowed(r, allowed) {
			w.Header().Add("Vary", "Origin")
			w.Header().Add("Vary", "Sec-Fetch-Site")
			envelope.WriteAPIError(w, r, http.StatusForbidden, "forbidden", "ORIGIN_FORBIDDEN", "Origin is not allowed to attach terminals", nil)
			return
		}
		// Attaching a terminal means typing into the person's shells and agent terminals. On the loopback
		// listener of a daemon the app started, that takes a ticket only the app could have asked for (a signed
		// POST); a LAN client has already proved itself with the connection password.
		if tickets != nil && !requestscope.IsLAN(r.Context()) && !tickets.Redeem(ticketFromSubprotocols(r)) {
			envelope.WriteAPIError(w, r, http.StatusForbidden, "forbidden", "TERMINAL_TICKET_REQUIRED", "Attaching a terminal needs a ticket from the AO app", nil)
			return
		}
		// The explicit exact-origin check above supports app://renderer and the
		// authenticated native mobile Origin, unlike the library's same-host rule.
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true, Subprotocols: []string{terminalticket.Subprotocol}})
		if err != nil {
			log.Warn("terminal mux: websocket upgrade failed", "err", err)
			return
		}
		c.SetReadLimit(terminalMuxReadLimit)
		mgr.Serve(r.Context(), &terminalMuxConn{c: c})
	}
}

// ticketFromSubprotocols reads the ticket the client offered as the subprotocol "ao.ticket.<ticket>". It is never
// read from the URL, where it would end up in logs and history.
func ticketFromSubprotocols(r *http.Request) string {
	for _, header := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, offered := range strings.Split(header, ",") {
			if ticket, ok := strings.CutPrefix(strings.TrimSpace(offered), terminalticket.SubprotocolPrefix); ok {
				return ticket
			}
		}
	}
	return ""
}

// terminalMuxConn adapts a coder/websocket connection to terminal.wsConn. JSON framing
// uses wsjson (text messages); Ping is a control frame; Close sends a normal
// closure.
type terminalMuxConn struct{ c *websocket.Conn }

func (a *terminalMuxConn) ReadJSON(ctx context.Context, v any) error { return wsjson.Read(ctx, a.c, v) }
func (a *terminalMuxConn) WriteJSON(ctx context.Context, v any) error {
	return wsjson.Write(ctx, a.c, v)
}
func (a *terminalMuxConn) Ping(ctx context.Context) error { return a.c.Ping(ctx) }
func (a *terminalMuxConn) Close(reason string) error {
	return a.c.Close(websocket.StatusNormalClosure, reason)
}

// Terminal attachment grants access to existing PTYs, so loopback/preview page
// origins must never inherit the general API's permissive CORS policy.
func terminalMuxRequestAllowed(r *http.Request, allowed map[string]struct{}) bool {
	if _, ok := previewutil.SessionIDFromHost(r.Host); ok {
		return false
	}
	if _, ok := previewutil.SessionIDFromArtifactHost(r.Host); ok {
		return false
	}
	if _, ok := previewutil.SessionIDFromInlineArtifactHost(r.Host); ok {
		return false
	}
	origins := r.Header.Values("Origin")
	if len(origins) == 0 {
		return r.Header.Get("Sec-Fetch-Site") == ""
	}
	if len(origins) != 1 || origins[0] == "" {
		return false
	}
	origin := origins[0]
	// Configuration names origins, never URL paths or opaque/wildcard origins.
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "app") || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(origin, " ,\t\r\n") {
		return false
	}
	if _, ok := allowed[origin]; ok {
		return true
	}
	// This compatibility Origin comes from React Native, not a browser grant.
	return origin == "http://localhost" && authenticatedLANBearer(r)
}
