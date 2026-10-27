package httpd

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/requestscope"
	"github.com/aoagents/agent-orchestrator/backend/internal/terminal"
	"github.com/aoagents/agent-orchestrator/backend/internal/terminalticket"
	"github.com/aoagents/agent-orchestrator/backend/internal/uicapability"
)

const ticketTestKey = "0123456789012345678901234567890123456789abc"

func ticketServer(t *testing.T, verifier *uicapability.Verifier) (*httptest.Server, string) {
	t.Helper()
	mgr := terminal.NewManager(&stubSource{argv: []string{"/bin/sh", "-c", "sleep 5"}}, nil, discardLogger())
	t.Cleanup(mgr.Close)
	ts := httptest.NewServer(NewRouterWithControl(config.Config{}, discardLogger(), mgr, APIDeps{UICapability: verifier}, ControlDeps{}))
	t.Cleanup(ts.Close)
	return ts, "ws" + strings.TrimPrefix(ts.URL, "http") + "/mux"
}

func askForTicket(t *testing.T, ts *httptest.Server, capability string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/terminal-tickets", nil)
	if err != nil {
		t.Fatal(err)
	}
	if capability != "" {
		req.Header.Set(uicapability.Header, capability)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	raw, _ := io.ReadAll(res.Body)
	var body struct {
		Ticket string `json:"ticket"`
	}
	_ = json.Unmarshal(raw, &body)
	return res.StatusCode, body.Ticket
}

func signedTicketRequest() string {
	return uicapability.Sign(ticketTestKey, uicapability.PurposeHumanWrite, uicapability.RequestSubject(http.MethodPost, "/api/v1/terminal-tickets"), time.Now())
}

func dialWith(url string, protocols ...string) (*websocket.Conn, *http.Response, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return websocket.Dial(ctx, url, &websocket.DialOptions{Subprotocols: protocols})
}

func TestADaemonTheAppStartedRequiresATicketToAttachATerminalAndAnAppTicketWorksOnce(t *testing.T) {
	ts, url := ticketServer(t, uicapability.New(ticketTestKey))

	if c, res, err := dialWith(url); err == nil {
		_ = c.CloseNow()
		t.Fatal("a terminal attached with no ticket")
	} else if res == nil || res.StatusCode != http.StatusForbidden {
		t.Fatalf("no ticket: %v, %v; want a 403", res, err)
	}

	status, ticket := askForTicket(t, ts, signedTicketRequest())
	if status != http.StatusCreated || len(ticket) < 40 {
		t.Fatalf("ticket request = %d %q", status, ticket)
	}
	c, res, err := dialWith(url, terminalticket.Subprotocol, terminalticket.SubprotocolPrefix+ticket)
	if err != nil {
		t.Fatalf("attach with a ticket: %v", err)
	}
	if c.Subprotocol() != terminalticket.Subprotocol || strings.Contains(res.Header.Get("Sec-WebSocket-Protocol"), ticket) {
		t.Fatalf("selected subprotocol %q, header %q: the ticket must never be echoed", c.Subprotocol(), res.Header.Get("Sec-WebSocket-Protocol"))
	}
	_ = c.CloseNow()

	if c, _, err := dialWith(url, terminalticket.Subprotocol, terminalticket.SubprotocolPrefix+ticket); err == nil {
		_ = c.CloseNow()
		t.Fatal("a ticket attached a second terminal")
	}
}

func TestATerminalTicketComesOnlyFromTheAppsCapabilityAndOnlyTheSubprotocolCountsAsOne(t *testing.T) {
	ts, url := ticketServer(t, uicapability.New(ticketTestKey))
	if status, _ := askForTicket(t, ts, ""); status != http.StatusForbidden {
		t.Fatalf("a ticket without the capability answered %d", status)
	}
	other := uicapability.Sign("another-run-key-another-run-key-another-run-key", uicapability.PurposeHumanWrite, uicapability.RequestSubject(http.MethodPost, "/api/v1/terminal-tickets"), time.Now())
	if status, _ := askForTicket(t, ts, other); status != http.StatusForbidden {
		t.Fatalf("a ticket for another run's key answered %d", status)
	}
	stale := uicapability.Sign(ticketTestKey, uicapability.PurposeHumanWrite, uicapability.RequestSubject(http.MethodPost, "/api/v1/terminal-tickets"), time.Now().Add(-5*time.Minute))
	if status, _ := askForTicket(t, ts, stale); status != http.StatusForbidden {
		t.Fatalf("a stale capability answered %d", status)
	}

	_, ticket := askForTicket(t, ts, signedTicketRequest())
	// In the URL, or as a protocol that is not the ticket prefix, it is not a ticket.
	for name, attempt := range map[string]func() (*websocket.Conn, *http.Response, error){
		"in the query":        func() (*websocket.Conn, *http.Response, error) { return dialWith(url + "?ticket=" + ticket) },
		"as a bare protocol":  func() (*websocket.Conn, *http.Response, error) { return dialWith(url, ticket) },
		"with a wrong prefix": func() (*websocket.Conn, *http.Response, error) { return dialWith(url, "ao.tick."+ticket) },
		"truncated": func() (*websocket.Conn, *http.Response, error) {
			return dialWith(url, terminalticket.SubprotocolPrefix+ticket[:10])
		},
		"a ticket nobody issued": func() (*websocket.Conn, *http.Response, error) {
			return dialWith(url, terminalticket.SubprotocolPrefix+strings.Repeat("A", 43))
		},
	} {
		if c, _, err := attempt(); err == nil {
			_ = c.CloseNow()
			t.Errorf("%s: a terminal attached", name)
		}
	}
	// None of those attempts used up the real ticket.
	if c, _, err := dialWith(url, terminalticket.Subprotocol, terminalticket.SubprotocolPrefix+ticket); err != nil {
		t.Fatalf("the real ticket after failed attempts: %v", err)
	} else {
		_ = c.CloseNow()
	}
}

func TestADaemonTheAppDidNotStartKeepsTheTerminalOpenAndIssuesNoTickets(t *testing.T) {
	ts, url := ticketServer(t, nil)
	c, _, err := dialWith(url)
	if err != nil {
		t.Fatalf("a headless daemon's terminal must stay as it was: %v", err)
	}
	_ = c.CloseNow()
	if status, _ := askForTicket(t, ts, signedTicketRequest()); status != http.StatusForbidden {
		t.Fatalf("a daemon with no key issued a ticket (%d)", status)
	}
}

func TestALANClientThatProvedItselfNeedsNoTicket(t *testing.T) {
	mgr := terminal.NewManager(&stubSource{argv: []string{"/bin/sh", "-c", "sleep 5"}}, nil, discardLogger())
	defer mgr.Close()
	handler := terminalMuxHandler(mgr, discardLogger(), nil, terminalticket.New(nil))
	asLAN := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r.WithContext(requestscope.WithLAN(r.Context())))
	})
	ts := httptest.NewServer(asLAN)
	defer ts.Close()
	c, _, err := dialWith("ws" + strings.TrimPrefix(ts.URL, "http"))
	if err != nil {
		t.Fatalf("a LAN-authenticated client was refused: %v", err)
	}
	_ = c.CloseNow()

	plain := httptest.NewServer(handler)
	defer plain.Close()
	if c, _, err := dialWith("ws" + strings.TrimPrefix(plain.URL, "http")); err == nil {
		_ = c.CloseNow()
		t.Fatal("a request that was not marked LAN attached without a ticket")
	}
}
