package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	"github.com/aoagents/agent-orchestrator/backend/internal/mobilebridge"
)

type stubTunnel struct{ endpoint *mobilebridge.TunnelEndpoint }

func (stubTunnel) Start(int)                                {}
func (stubTunnel) Stop()                                    {}
func (s stubTunnel) Endpoint() *mobilebridge.TunnelEndpoint { return s.endpoint }
func (stubTunnel) Status() mobilebridge.TunnelStatus        { return mobilebridge.TunnelStatus{} }

// The account directory publishes the one address every signed-in device can
// reach. The secure tailnet endpoint is tailnet-only, so it must never be
// selected in place of the public tunnel — with or without the tunnel up.
func TestAccountTunnelAddressSelectsOnlyTheTunnelNotSecureTailscale(t *testing.T) {
	const name = "box.tail1.ts.net"
	build := func(tunnel *mobilebridge.TunnelEndpoint) *controllers.BridgeService {
		path := mobilebridge.Path(t.TempDir())
		if err := mobilebridge.Save(path, mobilebridge.State{
			Enabled: true, Password: "secret12", LastPort: 3011,
			SecurePairing: true, AccountTokenHash: strings.Repeat("b", 64),
		}); err != nil {
			t.Fatal(err)
		}
		return &controllers.BridgeService{
			LAN:                &fakeLAN{started: true, port: 3011},
			ConfigPath:         path,
			Tunnel:             stubTunnel{endpoint: tunnel},
			PickLANHosts:       func() []string { return []string{"192.168.1.42"} },
			PickTailscaleHosts: func() []string { return []string{"100.72.46.7"} },
			QueryTS:            func() mobilebridge.TailscaleInfo { return mobilebridge.TailscaleInfo{Name: name, CertsEnabled: true} },
			ServeTarget:        func() int { return 3011 },
		}
	}

	withTunnel := build(&mobilebridge.TunnelEndpoint{Ready: true, Hostname: "x.trycloudflare.com"})
	eps := withTunnel.AdvertisedEndpoints()
	if len(eps) == 0 || eps[0].Kind != mobilebridge.KindTailscale || !eps[0].Secure || eps[0].Host != name {
		t.Fatalf("precondition: secure tailscale must lead the list, got %v", eps)
	}
	addr, hash := accountTunnelAddress(withTunnel)
	if addr != "https://x.trycloudflare.com:443" || hash == "" {
		t.Errorf("address = %q (hash set: %v), want the tunnel address", addr, hash != "")
	}

	noTunnel := build(nil)
	if addr, _ := accountTunnelAddress(noTunnel); addr != "" {
		t.Errorf("address = %q with no tunnel, want none (the tailnet route is not public)", addr)
	}
}

func TestPublishRemoteHostAddressUsesScopedDigestWithoutRedirects(t *testing.T) {
	hash := strings.Repeat("a", 64)
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.URL.Path != "/api/cloud/v1/remote-hosts/h_test/address" || r.Header.Get("Authorization") != "Bearer "+hash {
			t.Errorf("unexpected request: %s %s", r.URL.Path, r.Header.Get("Authorization"))
		}
		var input struct {
			URL string `json:"url"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.URL != "https://new.example.com:443" {
			t.Errorf("unexpected address: %+v (%v)", input, err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	if err := publishRemoteHostAddress(context.Background(), srv.Client(), srv.URL, "h_test", "https://new.example.com:443", hash); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("address was not published")
	}
}
