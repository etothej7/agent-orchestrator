package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/mobilebridge"
)

type fakeBridge struct{ enabled bool }

func (f *fakeBridge) Status() MobileStatusResponse {
	return MobileStatusResponse{Enabled: f.enabled, Host: "192.168.1.42", Port: 3011}
}
func (f *fakeBridge) StartRemoteAccess() (MobileStatusResponse, error) {
	return MobileStatusResponse{}, nil
}

func (f *fakeBridge) Enable() (MobileStatusResponse, error) {
	f.enabled = true
	r := f.Status()
	r.Password = "abcd1234"
	return r, nil
}
func (f *fakeBridge) EnableLANOnly() (MobileStatusResponse, error)    { return f.Enable() }
func (f *fakeBridge) EnableTunnelOnly() (MobileStatusResponse, error) { return f.Enable() }
func (f *fakeBridge) Disable() error                                  { f.enabled = false; return nil }
func (f *fakeBridge) Regenerate() (MobileStatusResponse, error) {
	r := f.Status()
	r.Password = "wxyz5678"
	return r, nil
}
func (f *fakeBridge) SetKeepAwake(on bool) (MobileStatusResponse, error) {
	r := f.Status()
	r.KeepAwake.Enabled = on
	return r, nil
}
func (f *fakeBridge) SetKeepAwakeLid(bool, bool) (MobileStatusResponse, error) {
	return MobileStatusResponse{}, errors.New("closed-lid wakefulness unavailable")
}
func (f *fakeBridge) SetSecurePairing(on bool) (MobileStatusResponse, error) {
	r := f.Status()
	r.SecurePairing.Enabled = on
	return r, nil
}
func (f *fakeBridge) SetBrowserRemoteAccess(on bool) (MobileStatusResponse, error) {
	r := f.Status()
	r.BrowserRemoteAccess = on
	return r, nil
}

func TestMobileBrowserRemoteAccess(t *testing.T) {
	c := &MobileController{Bridge: &fakeBridge{}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/mobile/browser-control", strings.NewReader(`{"enabled":true}`))
	rr := httptest.NewRecorder()
	c.BrowserRemoteAccess(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var got MobileStatusResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.BrowserRemoteAccess {
		t.Fatal("browser remote access was not enabled")
	}
}

// fakeLAN is a minimal LANController for exercising BridgeService directly.
type fakeLAN struct {
	running     bool
	hash        string
	accountHash string
	stopCalls   int
	bindHost    string
}

func (f *fakeLAN) Start(port int) (int, error) {
	f.running = true
	f.bindHost = "0.0.0.0"
	return port, nil
}
func (f *fakeLAN) StartLoopback(port int) (int, error) {
	f.running = true
	f.bindHost = "127.0.0.1"
	return port, nil
}
func (f *fakeLAN) Stop(ctx context.Context) error {
	f.stopCalls++
	f.running = false
	return nil
}
func (f *fakeLAN) Running() bool { return f.running }
func (f *fakeLAN) BoundPort() int {
	// Mirrors the real listener: no bound port until it is actually running.
	if !f.running {
		return 0
	}
	return 3011
}
func (f *fakeLAN) SetPasswordHash(h string)     { f.hash = h }
func (f *fakeLAN) PasswordHash() string         { return f.hash }
func (f *fakeLAN) SetAccountTokenHash(h string) { f.accountHash = h }
func (f *fakeLAN) AccountTokenHash() string     { return f.accountHash }

func TestIssueAccountTokenRequiresPairingPasswordAndPersistsHash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mobile", "config.json")
	if err := mobilebridge.Save(path, mobilebridge.State{Enabled: true, Password: "secret12"}); err != nil {
		t.Fatal(err)
	}
	lan := &fakeLAN{running: true}
	bridge := &BridgeService{LAN: lan, ConfigPath: path, HostID: "h_test"}
	if _, err := bridge.IssueAccountToken("wrong", "user-a"); err == nil {
		t.Fatal("wrong password accepted")
	}
	issued, err := bridge.IssueAccountToken("secret12", "user-a")
	if err != nil {
		t.Fatal(err)
	}
	if issued.HostID != "h_test" || len(issued.Token) != 64 {
		t.Fatalf("bad token: %+v", issued)
	}
	state, err := mobilebridge.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !mobilebridge.PasswordMatches(state.AccountTokenHash, issued.Token) || state.AccountToken != issued.Token || state.AccountTokenOwner != "user-a" || lan.accountHash != state.AccountTokenHash {
		t.Fatal("account credential was not persisted and armed")
	}
	if state.Password != "secret12" {
		t.Fatal("pairing password changed")
	}
}

func TestPasswordRotationRevokesAccountToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mobile", "config.json")
	lan := &fakeLAN{}
	bridge := &BridgeService{LAN: lan, ConfigPath: path, HostID: "h_test", DefaultPort: 3011}
	status, err := bridge.Enable()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bridge.IssueAccountToken(status.Password, "user-a"); err != nil {
		t.Fatal(err)
	}
	if lan.accountHash == "" {
		t.Fatal("account token was not armed")
	}
	if _, err := bridge.Regenerate(); err != nil {
		t.Fatal(err)
	}
	state, err := mobilebridge.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if state.AccountTokenHash != "" || lan.accountHash != "" {
		t.Fatal("password rotation left the account token valid")
	}
}

func TestIssueAccountTokenReusesCredentialForSameAccount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mobile", "config.json")
	if err := mobilebridge.Save(path, mobilebridge.State{Enabled: true, Password: "secret12"}); err != nil {
		t.Fatal(err)
	}
	lan := &fakeLAN{running: true}
	bridge := &BridgeService{LAN: lan, ConfigPath: path, HostID: "h_test"}
	first, err := bridge.IssueAccountToken("secret12", "user-a")
	if err != nil {
		t.Fatal(err)
	}
	second, err := bridge.IssueAccountToken("secret12", "user-a")
	if err != nil {
		t.Fatal(err)
	}
	if second.Token != first.Token {
		t.Fatalf("same-account retry rotated token: first=%q second=%q", first.Token, second.Token)
	}
	third, err := bridge.IssueAccountToken("secret12", "user-b")
	if err != nil {
		t.Fatal(err)
	}
	if third.Token == first.Token {
		t.Fatal("different account did not rotate token")
	}
	if got, err := bridge.IssueAccountToken(third.Token, "user-b"); err != nil || got.Token != third.Token {
		t.Fatalf("current token retry = %+v, %v; want current credential", got, err)
	}
}

// When Save fails during a fresh enable, the listener that Start already opened
// must be torn back down and the armed hash rolled back — otherwise a LAN
// listener stays live on 0.0.0.0 while persisted state/UI say enable failed.
func TestMobileEnableRollsBackListenerWhenSaveFails(t *testing.T) {
	// A ConfigPath whose parent is a regular file makes mobilebridge.Save's
	// MkdirAll (and thus Save) fail deterministically.
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	lan := &fakeLAN{}
	b := &BridgeService{LAN: lan, ConfigPath: filepath.Join(blocker, "mobile", "config.json"), DefaultPort: 3011}

	if _, err := b.Enable(); err == nil {
		t.Fatal("expected enable to fail on Save error")
	}
	if lan.Running() {
		t.Fatal("listener still running after failed enable; must be stopped")
	}
	if lan.stopCalls == 0 {
		t.Fatal("expected Stop to be called on rollback")
	}
	if lan.hash != "" {
		t.Fatalf("expected hash rolled back to empty, got %q", lan.hash)
	}
}

func TestMobileEnableReturnsPassword(t *testing.T) {
	c := &MobileController{Bridge: &fakeBridge{}}
	w := httptest.NewRecorder()
	c.Enable(w, httptest.NewRequest(http.MethodPost, "/api/v1/mobile/enable", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("got %d", w.Code)
	}
	var got MobileStatusResponse
	json.NewDecoder(w.Body).Decode(&got)
	if !got.Enabled || got.Password != "abcd1234" || got.Warning == "" {
		t.Fatalf("bad response: %+v", got)
	}
}

// Status must advertise both addresses so the renderer's LAN/Tailscale toggle
// can re-encode the pairing QR without a second round trip.
func TestMobileStatusSurfacesBothHosts(t *testing.T) {
	lan := &fakeLAN{running: true}
	b := &BridgeService{
		LAN:                lan,
		ConfigPath:         filepath.Join(t.TempDir(), "mobile", "config.json"),
		DefaultPort:        3011,
		PickLANHosts:       func() []string { return []string{"192.168.1.42"} },
		PickTailscaleHosts: func() []string { return []string{"100.72.46.7"} },
	}
	if _, err := b.Enable(); err != nil {
		t.Fatalf("enable: %v", err)
	}

	got := b.Status()
	if got.Host != "192.168.1.42" {
		t.Errorf("Host = %q want 192.168.1.42", got.Host)
	}
	if got.TailscaleHost != "100.72.46.7" {
		t.Errorf("TailscaleHost = %q want 100.72.46.7", got.TailscaleHost)
	}
}

// An absent Tailscale install is an empty string, not an error: the renderer
// uses "" to decide to show a hint instead of an unscannable QR.
func TestMobileStatusTailscaleHostEmptyWhenAbsent(t *testing.T) {
	b := &BridgeService{
		LAN:                &fakeLAN{running: true},
		ConfigPath:         filepath.Join(t.TempDir(), "mobile", "config.json"),
		DefaultPort:        3011,
		PickLANHosts:       func() []string { return []string{"192.168.1.42"} },
		PickTailscaleHosts: func() []string { return nil },
	}
	if _, err := b.Enable(); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if got := b.Status().TailscaleHost; got != "" {
		t.Errorf("TailscaleHost = %q want empty", got)
	}
}

// Unset pickers must fall back to the real autopickers rather than panicking on
// a nil func — production wiring in daemon.go leaves them unset. This also
// proves the call actually completed (not just "didn't panic"): the real
// AutopickLANIP/AutopickTailscaleIP paths run, and Port still comes through
// from the LAN controller untouched by that fallback.
func TestMobileStatusHostPickersDefaultWhenUnset(t *testing.T) {
	b := &BridgeService{
		LAN:         &fakeLAN{running: true},
		ConfigPath:  filepath.Join(t.TempDir(), "mobile", "config.json"),
		DefaultPort: 3011,
	}
	got := b.Status()
	if got.Port != 3011 {
		t.Errorf("Port = %d want 3011", got.Port)
	}
}

// The bridge falls back to an ephemeral port when its default is taken, so the
// proxy must be pointed at the port Start actually returned — not DefaultPort.
// This is the stale-port regression this feature exists to avoid.
func TestSecurePairingAppliesActualBoundPort(t *testing.T) {
	// Start returns the actual ephemeral-fallback port (54014); BoundPort is
	// deliberately stale (3011) so the test fails if the implementation
	// re-reads BoundPort instead of using the port Start returned.
	lan := &portOverrideLAN{startPort: 54014, boundPortStale: 3011}
	var applied []int
	b := &BridgeService{
		LAN:         lan,
		ConfigPath:  filepath.Join(t.TempDir(), "mobile", "config.json"),
		DefaultPort: 3011,
		ApplyServe:  func(port int) error { applied = append(applied, port); return nil },
		ClearServe:  func() error { return nil },
		QueryTS:     func() mobilebridge.TailscaleInfo { return tsUp },
		ServeTarget: func() int { return 54014 },
	}
	if _, err := b.SetSecurePairing(true); err != nil {
		t.Fatalf("SetSecurePairing: %v", err)
	}
	if _, err := b.Enable(); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if len(applied) == 0 || applied[len(applied)-1] != 54014 {
		t.Errorf("applied = %v, want the bound port 54014", applied)
	}
}

func TestSecurePairingStatusActive(t *testing.T) {
	b := newSecureBridge(t, tsUp, func() int { return 3011 })
	if _, err := b.SetSecurePairing(true); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Enable(); err != nil {
		t.Fatal(err)
	}
	sp := b.Status().SecurePairing
	if !sp.Enabled || !sp.Available || !sp.Active {
		t.Fatalf("securePairing = %+v, want enabled/available/active", sp)
	}
	if sp.Host != "prasads-macbook-pro.tail057d04.ts.net" || sp.Port != 443 {
		t.Errorf("host/port = %q/%d", sp.Host, sp.Port)
	}
	if sp.Reason != "" {
		t.Errorf("Reason = %q, want empty when available", sp.Reason)
	}
}

func TestSecurePairingReasons(t *testing.T) {
	cases := []struct {
		name   string
		info   mobilebridge.TailscaleInfo
		target func() int
		want   string
	}{
		{"no cli", mobilebridge.TailscaleInfo{}, func() int { return 0 }, "no_cli"},
		{"no certs", mobilebridge.TailscaleInfo{Name: "h.tail1.ts.net"}, func() int { return 0 }, "no_certs"},
		{"port mismatch", tsUp, func() int { return 9999 }, "port_mismatch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := newSecureBridge(t, tc.info, tc.target)
			if _, err := b.SetSecurePairing(true); err != nil {
				t.Fatal(err)
			}
			if _, err := b.Enable(); err != nil {
				t.Fatal(err)
			}
			if got := b.Status().SecurePairing.Reason; got != tc.want {
				t.Errorf("Reason = %q, want %q", got, tc.want)
			}
		})
	}
}

// A serve failure must leave the bridge running in plaintext mode rather than
// taking pairing down entirely.
func TestSecurePairingServeFailureKeepsBridgeUp(t *testing.T) {
	b := newSecureBridge(t, tsUp, func() int { return 0 })
	b.ApplyServe = func(int) error { return errors.New("port 443 in use") }
	if _, err := b.SetSecurePairing(true); err != nil {
		t.Fatal(err)
	}
	res, err := b.Enable()
	if err != nil {
		t.Fatalf("Enable must succeed despite serve failure: %v", err)
	}
	if !res.Enabled {
		t.Error("bridge not enabled")
	}
	if got := b.Status().SecurePairing.Reason; got != "serve_failed" {
		t.Errorf("Reason = %q, want serve_failed", got)
	}
}

// Turning the mode off must tear the proxy down, or it keeps pointing at a
// bridge the user believes is private.
func TestSecurePairingOffClearsProxy(t *testing.T) {
	cleared := 0
	b := newSecureBridge(t, tsUp, func() int { return 3011 })
	b.ClearServe = func() error { cleared++; return nil }
	if _, err := b.SetSecurePairing(false); err != nil || cleared != 0 {
		t.Fatalf("turning off unused secure pairing cleared an unrelated Serve route: clears=%d err=%v", cleared, err)
	}
	if _, err := b.SetSecurePairing(true); err != nil {
		t.Fatal(err)
	}
	if _, err := b.SetSecurePairing(false); err != nil {
		t.Fatal(err)
	}
	if cleared != 1 {
		t.Errorf("Clear called %d times, want 1", cleared)
	}
}

// If Clear fails when turning the mode off, the flag is already persisted
// off (so the API call must still succeed), but the proxy may still be live
// — Status must surface that as clear_failed rather than silently reporting
// disabled.
func TestSecurePairingOffClearFailureReported(t *testing.T) {
	b := newSecureBridge(t, tsUp, func() int { return 3011 })
	if _, err := b.SetSecurePairing(true); err != nil {
		t.Fatal(err)
	}
	b.ClearServe = func() error { return errors.New("tailscale serve clear: exit 1") }
	if _, err := b.SetSecurePairing(false); err != nil {
		t.Fatalf("SetSecurePairing(false) must not return an error: %v", err)
	}
	sp := b.Status().SecurePairing
	if sp.Enabled {
		t.Error("Enabled = true, want false")
	}
	if sp.Reason != "clear_failed" {
		t.Errorf("Reason = %q, want clear_failed", sp.Reason)
	}
}

var tsUp = mobilebridge.TailscaleInfo{
	Name:         "prasads-macbook-pro.tail057d04.ts.net",
	CertsEnabled: true,
}

// portOverrideLAN is fakeLAN with a caller-chosen bound port, so tests can
// exercise the ephemeral-fallback case. Start and BoundPort deliberately
// return different values: Start returns the freshly bound ephemeral port,
// while BoundPort returns a stale port a caller might have cached from
// before the fallback. Do not "fix" this by making them agree — the gap is
// what lets TestSecurePairingAppliesActualBoundPort catch a regression to
// re-reading b.LAN.BoundPort() instead of using the port Start returned.
type portOverrideLAN struct {
	fakeLAN
	startPort      int
	boundPortStale int
}

func (f *portOverrideLAN) BoundPort() int         { return f.boundPortStale }
func (f *portOverrideLAN) Start(int) (int, error) { f.running = true; return f.startPort, nil }

func newSecureBridge(t *testing.T, info mobilebridge.TailscaleInfo, target func() int) *BridgeService {
	t.Helper()
	return &BridgeService{
		LAN:         &fakeLAN{},
		ConfigPath:  filepath.Join(t.TempDir(), "mobile", "config.json"),
		DefaultPort: 3011,
		ApplyServe:  func(int) error { return nil },
		ClearServe:  func() error { return nil },
		QueryTS:     func() mobilebridge.TailscaleInfo { return info },
		ServeTarget: target,
	}
}

// `tailscale serve --https=443 off` is node-global: it removes whatever is on
// :443, not merely what AO put there. Disabling a bridge that never enabled
// secure pairing must therefore leave the tailnet proxy strictly alone, or AO
// silently destroys a serve route its user configured for themselves — or one
// belonging to another AO instance on the same node.
func TestDisableLeavesServeAloneWhenSecurePairingNeverEnabled(t *testing.T) {
	cleared := 0
	b := newSecureBridge(t, tsUp, func() int { return 3011 })
	b.ClearServe = func() error { cleared++; return nil }
	if _, err := b.Enable(); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if err := b.Disable(); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if cleared != 0 {
		t.Errorf("clearServe called %d times, want 0 — AO must not touch a proxy it never installed", cleared)
	}
}

// When AO did install the proxy, disabling must still remove it.
func TestDisableClearsServeWhenSecurePairingEnabled(t *testing.T) {
	cleared := 0
	b := newSecureBridge(t, tsUp, func() int { return 3011 })
	b.ClearServe = func() error { cleared++; return nil }
	if _, err := b.Enable(); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if _, err := b.SetSecurePairing(true); err != nil {
		t.Fatalf("set secure: %v", err)
	}
	if err := b.Disable(); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if cleared != 1 {
		t.Errorf("clearServe called %d times, want 1", cleared)
	}
}

// `tailscale serve --bg` outlives this process, so a graceful shutdown that
// stops only the listener leaves the tailnet routing to a local port with
// nothing authenticated behind it. Whatever binds that port next would be
// published to the tailnet in AO's place.
func TestShutdownServeClearsProxyWhenSecurePairingEnabled(t *testing.T) {
	cleared := 0
	b := newSecureBridge(t, tsUp, func() int { return 3011 })
	b.ClearServe = func() error { cleared++; return nil }
	if _, err := b.Enable(); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if _, err := b.SetSecurePairing(true); err != nil {
		t.Fatalf("set secure: %v", err)
	}
	b.ShutdownServe()
	if cleared != 1 {
		t.Errorf("clearServe called %d times on shutdown, want 1", cleared)
	}
}

// The preference must survive shutdown so boot restore re-applies the proxy
// against the next bound port without the user re-toggling anything.
func TestShutdownServeKeepsSecurePairingPreference(t *testing.T) {
	b := newSecureBridge(t, tsUp, func() int { return 3011 })
	if _, err := b.Enable(); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if _, err := b.SetSecurePairing(true); err != nil {
		t.Fatalf("set secure: %v", err)
	}
	b.ShutdownServe()
	st, err := mobilebridge.Load(b.ConfigPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !st.SecurePairing {
		t.Error("SecurePairing was cleared by shutdown; boot restore would not re-apply the proxy")
	}
}

// A shutdown with the bridge off, or with secure pairing never enabled, must
// not issue the node-global clear either.
func TestShutdownServeNoopWhenNotOwned(t *testing.T) {
	for name, setup := range map[string]func(*BridgeService){
		"bridge disabled":         func(b *BridgeService) {},
		"secure pairing never on": func(b *BridgeService) { _, _ = b.Enable() },
	} {
		t.Run(name, func(t *testing.T) {
			cleared := 0
			b := newSecureBridge(t, tsUp, func() int { return 3011 })
			b.ClearServe = func() error { cleared++; return nil }
			setup(b)
			b.ShutdownServe()
			if cleared != 0 {
				t.Errorf("clearServe called %d times, want 0", cleared)
			}
		})
	}
}

// The phone races every advertised endpoint, so Status must list all of them.
// Host/TailscaleHost keep reporting the first of each for the existing
// renderer, but they are now derived from the same lists rather than resolved
// separately, so the two can never disagree.
func TestMobileStatusAdvertisesEveryEndpoint(t *testing.T) {
	b := &BridgeService{
		LAN:                &fakeLAN{running: true},
		ConfigPath:         filepath.Join(t.TempDir(), "mobile", "config.json"),
		DefaultPort:        3011,
		PickLANHosts:       func() []string { return []string{"192.168.1.42", "10.0.0.5"} },
		PickTailscaleHosts: func() []string { return []string{"100.72.46.7"} },
	}
	if _, err := b.Enable(); err != nil {
		t.Fatalf("enable: %v", err)
	}

	got := b.Status()

	want := []mobilebridge.Endpoint{
		{Kind: mobilebridge.KindLAN, Host: "192.168.1.42", Port: 3011},
		{Kind: mobilebridge.KindLAN, Host: "10.0.0.5", Port: 3011},
		{Kind: mobilebridge.KindTailscale, Host: "100.72.46.7", Port: 3011},
	}
	if len(got.Endpoints) != len(want) {
		t.Fatalf("got %d endpoints %+v, want %d", len(got.Endpoints), got.Endpoints, len(want))
	}
	for i := range want {
		if got.Endpoints[i] != want[i] {
			t.Errorf("endpoint %d: got %+v want %+v", i, got.Endpoints[i], want[i])
		}
	}

	// The legacy singular fields stay in step with the lists.
	if got.Host != "192.168.1.42" {
		t.Errorf("Host = %q want the first LAN candidate", got.Host)
	}
	if got.TailscaleHost != "100.72.46.7" {
		t.Errorf("TailscaleHost = %q want the first Tailscale candidate", got.TailscaleHost)
	}
}

// No network at all is a real state. An empty list must not become a list
// containing an empty host, which the phone would try to dial.
func TestMobileStatusEndpointsEmptyWithoutNetwork(t *testing.T) {
	b := &BridgeService{
		LAN:                &fakeLAN{running: true},
		ConfigPath:         filepath.Join(t.TempDir(), "mobile", "config.json"),
		DefaultPort:        3011,
		PickLANHosts:       func() []string { return nil },
		PickTailscaleHosts: func() []string { return nil },
	}
	if _, err := b.Enable(); err != nil {
		t.Fatalf("enable: %v", err)
	}
	got := b.Status()
	if len(got.Endpoints) != 0 {
		t.Fatalf("got %+v, want no endpoints", got.Endpoints)
	}
	if got.Host != "" {
		t.Errorf("Host = %q want empty", got.Host)
	}
}

type fakeTunnel struct {
	startedOn int
	stops     int
	endpoint  *mobilebridge.TunnelEndpoint
	status    mobilebridge.TunnelStatus
}

func (f *fakeTunnel) Start(localPort int) { f.startedOn = localPort }
func (f *fakeTunnel) Stop()               { f.stops++ }
func (f *fakeTunnel) Endpoint() *mobilebridge.TunnelEndpoint {
	return f.endpoint
}
func (f *fakeTunnel) Status() mobilebridge.TunnelStatus { return f.status }

func TestMobileEnableStartsTheTunnelOnTheBoundPort(t *testing.T) {
	// The connector must target the port the listener actually bound, not the
	// configured default: Start falls back to an ephemeral port when the
	// default is taken, and a connector pointed at the wrong port tunnels
	// nothing.
	tun := &fakeTunnel{}
	b := &BridgeService{
		LAN:                &fakeLAN{},
		ConfigPath:         filepath.Join(t.TempDir(), "mobile", "config.json"),
		DefaultPort:        3011,
		PickLANHosts:       func() []string { return []string{"192.168.1.42"} },
		PickTailscaleHosts: func() []string { return nil },
		Tunnel:             tun,
	}
	if _, err := b.Enable(); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if tun.startedOn != 3011 {
		t.Fatalf("tunnel started on %d, want the bound port 3011", tun.startedOn)
	}
}

type stopErrorLAN struct {
	fakeLAN
	err          error
	stillRunning bool
}

func (f *stopErrorLAN) Stop(context.Context) error {
	f.stopCalls++
	f.running = f.stillRunning
	return f.err
}

func TestMobileDisableFinishesCleanupAfterStopErrorWhenListenerStopped(t *testing.T) {
	stopErr := errors.New("shutdown deadline exceeded")
	path := filepath.Join(t.TempDir(), "mobile.json")
	if err := mobilebridge.Save(path, mobilebridge.State{
		Enabled:       true,
		Password:      "paired-password",
		LastPort:      3011,
		SecurePairing: true,
	}); err != nil {
		t.Fatal(err)
	}
	cleared := 0
	lan := &stopErrorLAN{
		fakeLAN: fakeLAN{running: true, hash: mobilebridge.HashPassword("paired-password")},
		err:     stopErr,
	}
	bridge := &BridgeService{
		LAN:         lan,
		ConfigPath:  path,
		ClearServe:  func() error { cleared++; return nil },
		DefaultPort: 3011,
	}

	if err := bridge.Disable(); !errors.Is(err, stopErr) {
		t.Fatalf("Disable error = %v, want %v", err, stopErr)
	}
	state, err := mobilebridge.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if state.Enabled {
		t.Fatal("persisted bridge remained enabled after the listener stopped")
	}
	if cleared != 1 {
		t.Fatalf("owned secure-pairing proxy cleared %d times, want 1", cleared)
	}
}

func TestMobileDisablePreservesEnabledStateWhenStopErrorLeavesListenerRunning(t *testing.T) {
	stopErr := errors.New("listener still running")
	path := filepath.Join(t.TempDir(), "mobile.json")
	if err := mobilebridge.Save(path, mobilebridge.State{
		Enabled:       true,
		Password:      "paired-password",
		LastPort:      3011,
		SecurePairing: true,
	}); err != nil {
		t.Fatal(err)
	}
	cleared := 0
	lan := &stopErrorLAN{
		fakeLAN:      fakeLAN{running: true, hash: mobilebridge.HashPassword("paired-password")},
		err:          stopErr,
		stillRunning: true,
	}
	bridge := &BridgeService{
		LAN:         lan,
		ConfigPath:  path,
		ClearServe:  func() error { cleared++; return nil },
		DefaultPort: 3011,
	}

	if err := bridge.Disable(); !errors.Is(err, stopErr) {
		t.Fatalf("Disable error = %v, want %v", err, stopErr)
	}
	state, err := mobilebridge.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Enabled {
		t.Fatal("persisted bridge was disabled while the listener remained running")
	}
	if cleared != 0 {
		t.Fatalf("secure-pairing proxy cleared %d times while listener remained live", cleared)
	}
}

func TestMobileDisableStopsTheTunnel(t *testing.T) {
	// Leaving a public tunnel up after the user turned Connect Mobile off would
	// keep the machine reachable from the internet with the UI saying it is not.
	tun := &fakeTunnel{}
	b := &BridgeService{
		LAN:                &fakeLAN{},
		ConfigPath:         filepath.Join(t.TempDir(), "mobile", "config.json"),
		DefaultPort:        3011,
		PickLANHosts:       func() []string { return []string{"192.168.1.42"} },
		PickTailscaleHosts: func() []string { return nil },
		Tunnel:             tun,
	}
	if _, err := b.Enable(); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if err := b.Disable(); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if tun.stops != 1 {
		t.Fatalf("tunnel stopped %d times, want 1", tun.stops)
	}
}

func TestMobileStatusIncludesAReadyTunnelEndpoint(t *testing.T) {
	tun := &fakeTunnel{
		endpoint: &mobilebridge.TunnelEndpoint{Ready: true, Hostname: "abc.trycloudflare.com"},
		status:   mobilebridge.TunnelStatus{Running: true, Ready: true, Hostname: "abc.trycloudflare.com"},
	}
	b := &BridgeService{
		LAN:                &fakeLAN{running: true},
		ConfigPath:         filepath.Join(t.TempDir(), "mobile", "config.json"),
		DefaultPort:        3011,
		PickLANHosts:       func() []string { return []string{"192.168.1.42"} },
		PickTailscaleHosts: func() []string { return nil },
		Tunnel:             tun,
	}
	if _, err := b.Enable(); err != nil {
		t.Fatalf("enable: %v", err)
	}

	got := b.Status()
	last := got.Endpoints[len(got.Endpoints)-1]
	if last.Kind != mobilebridge.KindTunnel || last.Host != "abc.trycloudflare.com" {
		t.Fatalf("last endpoint = %+v, want the tunnel", last)
	}
	if !got.Tunnel.Ready {
		t.Error("Tunnel status not surfaced; the desktop cannot show progress")
	}
}

func TestMobileStatusOmitsATunnelThatIsNotReady(t *testing.T) {
	// Still settling. The desktop should say "preparing", and the QR must not
	// carry an address that answers 530.
	tun := &fakeTunnel{status: mobilebridge.TunnelStatus{Running: true}}
	b := &BridgeService{
		LAN:                &fakeLAN{running: true},
		ConfigPath:         filepath.Join(t.TempDir(), "mobile", "config.json"),
		DefaultPort:        3011,
		PickLANHosts:       func() []string { return []string{"192.168.1.42"} },
		PickTailscaleHosts: func() []string { return nil },
		Tunnel:             tun,
	}
	if _, err := b.Enable(); err != nil {
		t.Fatalf("enable: %v", err)
	}

	for _, e := range b.Status().Endpoints {
		if e.Kind == mobilebridge.KindTunnel {
			t.Fatalf("advertised %+v while the tunnel was not ready", e)
		}
	}
}

func TestMobileWorksWithNoTunnelConfigured(t *testing.T) {
	// Remote access unavailable (no cloudflared, or the feature off) must leave
	// the LAN bridge behaving exactly as it did before.
	b := &BridgeService{
		LAN:                &fakeLAN{running: true},
		ConfigPath:         filepath.Join(t.TempDir(), "mobile", "config.json"),
		DefaultPort:        3011,
		PickLANHosts:       func() []string { return []string{"192.168.1.42"} },
		PickTailscaleHosts: func() []string { return nil },
	}
	if _, err := b.Enable(); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if got := b.Status(); len(got.Endpoints) != 1 || got.Endpoints[0].Kind != mobilebridge.KindLAN {
		t.Fatalf("endpoints = %+v, want just the LAN one", got.Endpoints)
	}
	if err := b.Disable(); err != nil {
		t.Fatalf("disable: %v", err)
	}
}

// The desktop builds the pairing code, and the code must carry the machine's
// identity so the phone can verify every endpoint it later races. Without it
// here the renderer would have to make a second call to /api/v1/identity just
// to draw a QR.
func TestMobileStatusCarriesTheHostIdentity(t *testing.T) {
	b := &BridgeService{
		LAN:                &fakeLAN{running: true},
		ConfigPath:         filepath.Join(t.TempDir(), "mobile", "config.json"),
		DefaultPort:        3011,
		HostID:             "h_b3e07f31",
		PickLANHosts:       func() []string { return []string{"192.168.1.42"} },
		PickTailscaleHosts: func() []string { return nil },
	}
	if _, err := b.Enable(); err != nil {
		t.Fatalf("enable: %v", err)
	}

	if got := b.Status().HostID; got != "h_b3e07f31" {
		t.Fatalf("HostID = %q want h_b3e07f31", got)
	}
}

// Found by running the daemon: with Connect Mobile off there is no bound port,
// so every endpoint was advertised as host:0. A phone would dutifully race
// addresses that cannot work. Nothing is reachable until the bridge is up, so
// the honest answer is an empty list.
func TestMobileAdvertisesNothingWhileTheBridgeIsOff(t *testing.T) {
	b := &BridgeService{
		LAN:                &fakeLAN{running: false},
		ConfigPath:         filepath.Join(t.TempDir(), "mobile", "config.json"),
		DefaultPort:        3011,
		PickLANHosts:       func() []string { return []string{"192.168.1.42"} },
		PickTailscaleHosts: func() []string { return []string{"100.72.46.7"} },
	}

	if got := b.AdvertisedEndpoints(); len(got) != 0 {
		t.Fatalf("advertised %+v while the bridge was disabled", got)
	}
	if got := b.Status().Endpoints; len(got) != 0 {
		t.Fatalf("status advertised %+v while the bridge was disabled", got)
	}
}

// Found by running the desktop app: enabling Connect Mobile started the
// connector, but restarting the daemon restored only the LAN listener. Remote
// access then stayed silently off — the UI showed the bridge enabled while the
// tunnel never came back — until the user toggled it off and on.
//
// A restart does not go through enableWithPasswordLocked (there is no password to
// rotate), so RestoreOnBoot has to mirror its post-Start work.
func TestMobileRestoreOnBootStartsTheTunnel(t *testing.T) {
	tun := &fakeTunnel{}
	b := &BridgeService{
		LAN:                &fakeLAN{},
		ConfigPath:         filepath.Join(t.TempDir(), "mobile", "config.json"),
		DefaultPort:        3011,
		PickLANHosts:       func() []string { return []string{"192.168.1.42"} },
		PickTailscaleHosts: func() []string { return nil },
		Tunnel:             tun,
	}

	if err := b.RestoreOnBoot(mobilebridge.State{Enabled: true, Password: "pw", LastPort: 3011}); err != nil {
		t.Fatalf("restore: %v", err)
	}

	if tun.startedOn != 3011 {
		t.Fatalf("tunnel started on %d, want the restored port 3011", tun.startedOn)
	}
}

func TestMobileRestoreOnBootWithoutATunnelIsHarmless(t *testing.T) {
	b := &BridgeService{
		LAN:                &fakeLAN{},
		ConfigPath:         filepath.Join(t.TempDir(), "mobile", "config.json"),
		DefaultPort:        3011,
		PickLANHosts:       func() []string { return []string{"192.168.1.42"} },
		PickTailscaleHosts: func() []string { return nil },
	}
	if err := b.RestoreOnBoot(mobilebridge.State{Enabled: true, Password: "pw", LastPort: 3011}); err != nil {
		t.Fatalf("restore: %v", err)
	}
}

// A cloudflared process outlives the daemon that spawned it, so quitting
// without stopping it leaves a public hostname pointing at a port that no
// longer has the authenticated LAN listener behind it. Reaping on the next
// boot cleans it up eventually, but "eventually" is whenever the user happens
// to reopen the app — until then the process runs and the hostname resolves.
func TestShutdownTunnelStopsTheConnector(t *testing.T) {
	dir := t.TempDir()
	tun := &fakeTunnel{}
	b := &BridgeService{ConfigPath: filepath.Join(dir, "mobile.json"), Tunnel: tun}

	b.ShutdownTunnel()

	if tun.stops != 1 {
		t.Fatalf("connector stops = %d, want 1", tun.stops)
	}
}

// Remote access is optional: without cloudflared installed there is no
// connector, and shutdown must not panic on the way out.
func TestShutdownTunnelWithoutAConnector(t *testing.T) {
	dir := t.TempDir()
	b := &BridgeService{ConfigPath: filepath.Join(dir, "mobile.json")}

	b.ShutdownTunnel()
}

// Remote access is optional: without cloudflared there is no connector at all.
// A zero TunnelStatus made that indistinguishable from "not started yet", so
// the desktop showed a normal QR and the user had no way to learn that
// connecting from cellular was simply unavailable on this machine.
func TestTunnelStatusReportsWhenRemoteAccessIsUnsupported(t *testing.T) {
	dir := t.TempDir()
	b := &BridgeService{ConfigPath: filepath.Join(dir, "mobile.json")}

	st := b.tunnelStatus()

	if st.Supported {
		t.Fatal("no connector configured, so remote access is not supported")
	}
}

func TestTunnelStatusReportsSupportedWhenAConnectorExists(t *testing.T) {
	dir := t.TempDir()
	b := &BridgeService{ConfigPath: filepath.Join(dir, "mobile.json"), Tunnel: &fakeTunnel{}}

	if !b.tunnelStatus().Supported {
		t.Fatal("a configured connector means remote access is supported")
	}
}

// Resolution happens once at daemon start, so a cloudflared installed from
// Connect Mobile was invisible until AO restarted — the user pressed Install,
// watched it succeed, and remote access stayed off with no explanation.
// Enabling is the natural moment to look again.
func TestEnableResolvesAConnectorInstalledSinceBoot(t *testing.T) {
	dir := t.TempDir()
	tun := &fakeTunnel{}
	resolved := 0
	b := &BridgeService{
		ConfigPath:  filepath.Join(dir, "mobile.json"),
		LAN:         &fakeLAN{},
		DefaultPort: 3011,
		ResolveTunnel: func() TunnelController {
			resolved++
			return tun
		},
	}

	if _, err := b.Enable(); err != nil {
		t.Fatalf("enable: %v", err)
	}

	if resolved != 1 {
		t.Fatalf("resolver calls = %d, want 1", resolved)
	}
	if tun.startedOn != 3011 {
		t.Fatalf("connector started on %d, want the bound port 3011", tun.startedOn)
	}
}

// A machine that still has no cloudflared must enable cleanly as a LAN-only
// bridge rather than failing: remote access is optional.
func TestEnableWithoutAConnectorStillWorks(t *testing.T) {
	dir := t.TempDir()
	b := &BridgeService{
		ConfigPath:    filepath.Join(dir, "mobile.json"),
		LAN:           &fakeLAN{},
		DefaultPort:   3011,
		ResolveTunnel: func() TunnelController { return nil },
	}

	if _, err := b.Enable(); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if b.tunnelStatus().Supported {
		t.Fatal("no connector resolved, so remote access is not supported")
	}
}

// Installing cloudflared used to re-enable the bridge so the daemon would look
// for the new binary. Enable always mints a fresh password, so that silently
// invalidated the phone already paired — the user installed remote access and
// lost the connection they were setting it up for.
func TestStartRemoteAccessKeepsThePairedPhoneWorking(t *testing.T) {
	dir := t.TempDir()
	lan := &fakeLAN{}
	tun := &fakeTunnel{}
	b := &BridgeService{
		ConfigPath: filepath.Join(dir, "mobile.json"), LAN: lan, DefaultPort: 3011,
	}
	before, err := b.Enable()
	if err != nil {
		t.Fatalf("enable: %v", err)
	}

	// cloudflared appears only now, as an install would leave it.
	b.ResolveTunnel = func() TunnelController { return tun }
	after, err := b.StartRemoteAccess()
	if err != nil {
		t.Fatalf("StartRemoteAccess: %v", err)
	}

	if after.Password != before.Password {
		t.Fatal("password rotated: the phone paired a moment ago can no longer authenticate")
	}
	if tun.startedOn != 3011 {
		t.Fatalf("connector started on %d, want the bound port 3011", tun.startedOn)
	}
}

// Nothing to start, and nothing to break: the bridge is off, so this is a
// no-op rather than an error the UI has to special-case.
func TestStartRemoteAccessWhileDisabledDoesNothing(t *testing.T) {
	dir := t.TempDir()
	tun := &fakeTunnel{}
	b := &BridgeService{
		ConfigPath: filepath.Join(dir, "mobile.json"), LAN: &fakeLAN{}, DefaultPort: 3011,
		ResolveTunnel: func() TunnelController { return tun },
	}

	if _, err := b.StartRemoteAccess(); err != nil {
		t.Fatalf("StartRemoteAccess: %v", err)
	}
	if tun.startedOn != 0 {
		t.Fatal("connector started while the bridge is disabled")
	}
}

type startErrorLAN struct {
	fakeLAN
	err error
}

func (f *startErrorLAN) Start(int) (int, error) { return 0, f.err }

func TestMobileRestoreOnBootRollsBackHashWhenStartFails(t *testing.T) {
	startErr := errors.New("bind failed")
	lan := &startErrorLAN{fakeLAN: fakeLAN{hash: "previous-hash", accountHash: "previous-account-hash"}, err: startErr}
	bridge := &BridgeService{LAN: lan}

	err := bridge.RestoreOnBoot(mobilebridge.State{Enabled: true, Password: "restored-password", AccountTokenHash: "restored-account-hash", LastPort: 3011})
	if !errors.Is(err, startErr) {
		t.Fatalf("RestoreOnBoot error = %v, want %v", err, startErr)
	}
	if got := lan.PasswordHash(); got != "previous-hash" {
		t.Fatalf("password hash after failed restore = %q, want previous-hash", got)
	}
	if got := lan.AccountTokenHash(); got != "previous-account-hash" {
		t.Fatalf("account token hash after failed restore = %q, want previous-account-hash", got)
	}
}

// secureEndpointBridge is a running bridge on port 3011 with stubbed address
// scans, so endpoint lists are independent of the host's real interfaces.
func secureEndpointBridge(t *testing.T, info mobilebridge.TailscaleInfo, target func() int) *BridgeService {
	t.Helper()
	b := newSecureBridge(t, info, target)
	b.PickLANHosts = func() []string { return []string{"192.168.1.42"} }
	b.PickTailscaleHosts = func() []string { return []string{"100.72.46.7"} }
	return b
}

func assertBothEndpointLists(t *testing.T, b *BridgeService, want []mobilebridge.Endpoint) {
	t.Helper()
	for name, got := range map[string][]mobilebridge.Endpoint{
		"offer (Status.Endpoints)":      b.Status().Endpoints,
		"refresh (AdvertisedEndpoints)": b.AdvertisedEndpoints(),
	} {
		if len(got) != len(want) {
			t.Fatalf("%s = %v, want %v", name, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s[%d] = %+v, want %+v", name, i, got[i], want[i])
			}
		}
	}
}

var (
	rawTailscale    = mobilebridge.Endpoint{Kind: mobilebridge.KindTailscale, Host: "100.72.46.7", Port: 3011}
	lanEndpoint     = mobilebridge.Endpoint{Kind: mobilebridge.KindLAN, Host: "192.168.1.42", Port: 3011}
	secureTailscale = mobilebridge.Endpoint{Kind: mobilebridge.KindTailscale, Host: tsUp.Name, Port: 443, Secure: true}
)

func TestSecurePairingAdvertisesSecureTailscaleInOfferAndRefresh(t *testing.T) {
	b := secureEndpointBridge(t, tsUp, func() int { return 3011 })
	if _, err := b.SetSecurePairing(true); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Enable(); err != nil {
		t.Fatal(err)
	}
	// Secure first, LAN next, and no plaintext 100.x entry beside the TLS one.
	assertBothEndpointLists(t, b, []mobilebridge.Endpoint{secureTailscale, lanEndpoint})
}

func TestSecurePairingAdvertisesSecureTailscaleAheadOfTheTunnel(t *testing.T) {
	b := secureEndpointBridge(t, tsUp, func() int { return 3011 })
	b.Tunnel = &fakeTunnel{endpoint: &mobilebridge.TunnelEndpoint{Ready: true, Hostname: "x.trycloudflare.com"}}
	if _, err := b.SetSecurePairing(true); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Enable(); err != nil {
		t.Fatal(err)
	}
	assertBothEndpointLists(t, b, []mobilebridge.Endpoint{
		secureTailscale, lanEndpoint,
		{Kind: mobilebridge.KindTunnel, Host: "x.trycloudflare.com", Port: 443, Secure: true},
	})
}

func TestSecurePairingOffKeepsRawTailscaleEndpoint(t *testing.T) {
	b := secureEndpointBridge(t, tsUp, func() int { return 3011 })
	if _, err := b.Enable(); err != nil {
		t.Fatal(err)
	}
	assertBothEndpointLists(t, b, []mobilebridge.Endpoint{lanEndpoint, rawTailscale})
}

// While the TLS route cannot be verified, the raw address stays advertised —
// dropping it would leave an older phone with no tailnet route at all — and the
// secure entry is never invented.
func TestSecurePairingNotActiveDoesNotAdvertiseSecureTailscale(t *testing.T) {
	cases := []struct {
		name   string
		info   mobilebridge.TailscaleInfo
		target func() int
		apply  func(int) error
	}{
		{"no cli", mobilebridge.TailscaleInfo{}, func() int { return 3011 }, nil},
		{"no certs", mobilebridge.TailscaleInfo{Name: tsUp.Name}, func() int { return 3011 }, nil},
		{"port mismatch", tsUp, func() int { return 9999 }, nil},
		{"foreign serve target", tsUp, func() int { return 0 }, nil},
		{"serve apply failed", tsUp, func() int { return 3011 }, func(int) error { return errors.New("boom") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := secureEndpointBridge(t, tc.info, tc.target)
			if tc.apply != nil {
				b.ApplyServe = tc.apply
			}
			if _, err := b.SetSecurePairing(true); err != nil {
				t.Fatal(err)
			}
			if _, err := b.Enable(); err != nil {
				t.Fatal(err)
			}
			assertBothEndpointLists(t, b, []mobilebridge.Endpoint{lanEndpoint, rawTailscale})
		})
	}
}

func TestSecurePairingEndpointNeedsARunningBridge(t *testing.T) {
	b := secureEndpointBridge(t, tsUp, func() int { return 3011 })
	if _, err := b.SetSecurePairing(true); err != nil {
		t.Fatal(err)
	}
	if got := b.AdvertisedEndpoints(); len(got) != 0 {
		t.Fatalf("endpoints = %v, want none while the bridge is off", got)
	}
}

// --- one-time secure-pairing migration ---

// migrationBridge is a bridge persisted as enabled with secure pairing never
// chosen, plus counters for the node-global side effects.
type migrationBridge struct {
	*BridgeService
	applied  []int
	cleared  int
	inUseHit int
}

func newMigrationBridge(t *testing.T, info mobilebridge.TailscaleInfo, inUse func() (bool, error), st mobilebridge.State) *migrationBridge {
	t.Helper()
	mb := &migrationBridge{BridgeService: newSecureBridge(t, info, func() int { return 3011 })}
	mb.ApplyServe = func(p int) error { mb.applied = append(mb.applied, p); return nil }
	mb.ClearServe = func() error { mb.cleared++; return nil }
	mb.ServeInUse = func() (bool, error) { mb.inUseHit++; return inUse() }
	st.Enabled, st.Password, st.LastPort = true, "secret12", 3011
	if err := mobilebridge.Save(mb.ConfigPath, st); err != nil {
		t.Fatal(err)
	}
	return mb
}

func empty() (bool, error) { return false, nil }

func loadState(t *testing.T, b *BridgeService) mobilebridge.State {
	t.Helper()
	st, err := mobilebridge.Load(b.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestMigrateSecurePairingEnablesOnceWhenEverythingIsReady(t *testing.T) {
	mb := newMigrationBridge(t, tsUp, empty, mobilebridge.State{})
	if got := mb.MigrateSecurePairingV1(); got != SecurePairingMigrationEnabled {
		t.Fatalf("outcome = %v, want enabled", got)
	}
	st := loadState(t, mb.BridgeService)
	if !st.SecurePairing || !st.SecurePairingAutoEnabledV1 {
		t.Fatalf("state = %+v, want secure pairing on with the marker set", st)
	}
	// Boot restore then applies serve against the bound port.
	if err := mb.RestoreOnBoot(st); err != nil {
		t.Fatal(err)
	}
	if len(mb.applied) != 1 || mb.applied[0] != 3011 {
		t.Errorf("applied = %v, want one apply on the bound port", mb.applied)
	}
	// Second boot: the marker makes it a no-op.
	mb.inUseHit = 0
	if got := mb.MigrateSecurePairingV1(); got != SecurePairingMigrationDone {
		t.Errorf("second run = %v, want done", got)
	}
	if mb.inUseHit != 0 {
		t.Error("second run probed the serve config again")
	}
}

// An explicit "off" is a decision: it must survive a daemon restart, even when
// the user made it before this build ever ran the migration.
func TestMigrateSecurePairingRespectsExplicitOff(t *testing.T) {
	mb := newMigrationBridge(t, tsUp, empty, mobilebridge.State{})
	if _, err := mb.SetSecurePairing(false); err != nil {
		t.Fatal(err)
	}
	if got := mb.MigrateSecurePairingV1(); got != SecurePairingMigrationDone {
		t.Fatalf("outcome = %v, want done (marker set by the explicit choice)", got)
	}
	if loadState(t, mb.BridgeService).SecurePairing {
		t.Error("migration turned secure pairing back on after an explicit off")
	}
}

func TestMigrateSecurePairingOnThenOffThenRestartStaysOff(t *testing.T) {
	mb := newMigrationBridge(t, tsUp, empty, mobilebridge.State{})
	mb.MigrateSecurePairingV1()
	if _, err := mb.SetSecurePairing(false); err != nil {
		t.Fatal(err)
	}
	mb.MigrateSecurePairingV1()
	if loadState(t, mb.BridgeService).SecurePairing {
		t.Error("secure pairing came back after the user turned it off")
	}
}

func TestMigrateSecurePairingLeavesExistingOptInAlone(t *testing.T) {
	mb := newMigrationBridge(t, tsUp, empty, mobilebridge.State{SecurePairing: true})
	if got := mb.MigrateSecurePairingV1(); got != SecurePairingMigrationDone {
		t.Fatalf("outcome = %v, want done", got)
	}
	if mb.inUseHit != 0 || len(mb.applied) != 0 || mb.cleared != 0 {
		t.Errorf("touched the serve config for an already-on user: probes=%d applies=%v clears=%d", mb.inUseHit, mb.applied, mb.cleared)
	}
	if !loadState(t, mb.BridgeService).SecurePairingAutoEnabledV1 {
		t.Error("marker not recorded")
	}
}

// A foreign :443 serve config is never replaced or cleared. The refusal is not
// final — nothing is marked — and Status says why while the mode is off.
func TestMigrateSecurePairingRefusesForeignServeConfig(t *testing.T) {
	mb := newMigrationBridge(t, tsUp, func() (bool, error) { return true, nil }, mobilebridge.State{})
	if got := mb.MigrateSecurePairingV1(); got != SecurePairingMigrationForeignServe {
		t.Fatalf("outcome = %v, want foreign serve refusal", got)
	}
	st := loadState(t, mb.BridgeService)
	if st.SecurePairing || st.SecurePairingAutoEnabledV1 {
		t.Errorf("state = %+v, want untouched", st)
	}
	if len(mb.applied) != 0 || mb.cleared != 0 {
		t.Errorf("modified serve: applies=%v clears=%d", mb.applied, mb.cleared)
	}
	if got := mb.Status().SecurePairing.Reason; got != "foreign_serve" {
		t.Errorf("Reason = %q, want foreign_serve", got)
	}
}

// If the serve config cannot be read we cannot prove it is empty, so refuse.
func TestMigrateSecurePairingRefusesWhenServeConfigUnreadable(t *testing.T) {
	mb := newMigrationBridge(t, tsUp, func() (bool, error) { return false, errors.New("exit 1") }, mobilebridge.State{})
	if got := mb.MigrateSecurePairingV1(); got != SecurePairingMigrationForeignServe {
		t.Fatalf("outcome = %v, want refusal", got)
	}
	if loadState(t, mb.BridgeService).SecurePairing {
		t.Error("enabled despite an unreadable serve config")
	}
}

func TestMigrateSecurePairingWaitsUntilTailscaleIsReady(t *testing.T) {
	cases := []struct {
		name string
		info mobilebridge.TailscaleInfo
		st   mobilebridge.State
		want SecurePairingMigrationOutcome
	}{
		{"no cli", mobilebridge.TailscaleInfo{}, mobilebridge.State{}, SecurePairingMigrationNotReady},
		{"no certs", mobilebridge.TailscaleInfo{Name: tsUp.Name}, mobilebridge.State{}, SecurePairingMigrationNotReady},
		{"tunnel-only", tsUp, mobilebridge.State{LoopbackOnly: true}, SecurePairingMigrationNotReady},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mb := newMigrationBridge(t, tc.info, empty, tc.st)
			if got := mb.MigrateSecurePairingV1(); got != tc.want {
				t.Fatalf("outcome = %v, want %v", got, tc.want)
			}
			st := loadState(t, mb.BridgeService)
			if st.SecurePairing || st.SecurePairingAutoEnabledV1 {
				t.Errorf("state = %+v, want untouched so a later boot can retry", st)
			}
		})
	}
}

func TestMigrateSecurePairingSkipsWhenMobileIsOff(t *testing.T) {
	mb := newMigrationBridge(t, tsUp, empty, mobilebridge.State{})
	st := loadState(t, mb.BridgeService)
	st.Enabled = false
	if err := mobilebridge.Save(mb.ConfigPath, st); err != nil {
		t.Fatal(err)
	}
	if got := mb.MigrateSecurePairingV1(); got != SecurePairingMigrationNotReady {
		t.Fatalf("outcome = %v, want not ready", got)
	}
	if loadState(t, mb.BridgeService).SecurePairingAutoEnabledV1 {
		t.Error("marker set while mobile was off")
	}
}

// Enable rebuilds the persisted state; the marker must ride along or an
// explicit choice would be forgotten on the next password rotation.
func TestSecurePairingMarkerSurvivesEnableAndRotation(t *testing.T) {
	b := newSecureBridge(t, tsUp, func() int { return 3011 })
	if _, err := b.SetSecurePairing(false); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Enable(); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Regenerate(); err != nil {
		t.Fatal(err)
	}
	if !loadState(t, b).SecurePairingAutoEnabledV1 {
		t.Error("marker lost across enable/regenerate")
	}
}

// --- ownership, funnel and caching, through a stateful fake tailscale CLI ---

// fakeTailscale is a tiny stand-in for the CLI: it keeps the node-global :443
// config in memory so tests can see what AO read, wrote and left alone.
type fakeTailscale struct {
	cfg    string // current `serve status --json` output
	writes []string
	reads  map[string]int
}

func (f *fakeTailscale) run(_ context.Context, args ...string) ([]byte, error) {
	if f.reads == nil {
		f.reads = map[string]int{}
	}
	key := strings.Join(args, " ")
	switch {
	case key == "status --json":
		f.reads[key]++
		return []byte(`{"Self":{"DNSName":"` + tsUp.Name + `."},"CertDomains":["` + tsUp.Name + `"]}`), nil
	case key == "serve status --json":
		f.reads[key]++
		return []byte(f.cfg), nil
	case strings.HasPrefix(key, "serve --bg --https=443 http://127.0.0.1:"):
		f.writes = append(f.writes, key)
		port := strings.TrimPrefix(key, "serve --bg --https=443 http://127.0.0.1:")
		f.cfg = proxyCfg(port)
		return nil, nil
	case key == "serve --https=443 off":
		f.writes = append(f.writes, key)
		f.cfg = `{}`
		return nil, nil
	}
	return nil, errors.New("unexpected tailscale call: " + key)
}

func proxyCfg(port string) string {
	return `{"TCP":{"443":{"HTTPS":true}},"Web":{"` + tsUp.Name + `:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:` + port + `"}}}}}`
}

const (
	staticCfg = `{"TCP":{"443":{"HTTPS":true}},"Web":{"` + "prasads-macbook-pro.tail057d04.ts.net" + `:443":{"Handlers":{"/":{"Text":"hi"}}}}}`
	funnelCfg = `{"AllowFunnel":{"prasads-macbook-pro.tail057d04.ts.net:443":true}}`
)

// cliBridge is a bridge whose only tailscale access is the fake CLI.
func cliBridge(t *testing.T, cfg string) (*BridgeService, *fakeTailscale) {
	t.Helper()
	f := &fakeTailscale{cfg: cfg}
	b := &BridgeService{
		LAN:                &fakeLAN{},
		ConfigPath:         filepath.Join(t.TempDir(), "mobile", "config.json"),
		DefaultPort:        3011,
		PickLANHosts:       func() []string { return []string{"192.168.1.42"} },
		PickTailscaleHosts: func() []string { return []string{"100.72.46.7"} },
		TailscaleRun:       f.run,
	}
	return b, f
}

func TestSecurePairingAppliesToEmptyServeAndRecordsOwnership(t *testing.T) {
	b, f := cliBridge(t, `{}`)
	mustNoErr(t, func() error { _, err := b.SetSecurePairing(true); return err })
	mustNoErr(t, func() error { _, err := b.Enable(); return err })
	if len(f.writes) != 1 || f.writes[0] != "serve --bg --https=443 http://127.0.0.1:3011" {
		t.Fatalf("writes = %v", f.writes)
	}
	if got := loadState(t, b).ServeAppliedPort; got != 3011 {
		t.Errorf("ownership record = %d, want 3011", got)
	}
	if !b.Status().SecurePairing.Active {
		t.Error("not active after a clean apply")
	}
}

func TestSecurePairingNeverOverwritesForeignOrFunnelConfig(t *testing.T) {
	cases := []struct {
		name, cfg, reason string
	}{
		{"static site", staticCfg, "foreign_serve"},
		{"proxy to someone else's port", proxyCfg("4999"), "foreign_serve"},
		{"funnel flag", funnelCfg, "funnel_exposed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, f := cliBridge(t, tc.cfg)
			mustNoErr(t, func() error { _, err := b.SetSecurePairing(true); return err })
			if _, err := b.Enable(); err != nil {
				t.Fatalf("Enable must keep the bridge up: %v", err)
			}
			if len(f.writes) != 0 {
				t.Fatalf("modified the node's serve config: %v", f.writes)
			}
			if f.cfg != tc.cfg {
				t.Error("serve config changed")
			}
			sp := b.Status().SecurePairing
			if sp.Reason != tc.reason || sp.Active {
				t.Errorf("securePairing = %+v, want reason %s and not active", sp, tc.reason)
			}
			for _, e := range b.AdvertisedEndpoints() {
				if e.Secure && e.Kind == mobilebridge.KindTailscale {
					t.Error("advertised a secure tailnet route that was never applied")
				}
			}
			if got := loadState(t, b).ServeAppliedPort; got != 0 {
				t.Errorf("ownership record = %d for a config AO did not apply", got)
			}
		})
	}
}

// Port change while the mode is on: the live proxy is AO's own (recorded), so it
// is re-pointed rather than refused.
func TestSecurePairingRepointsItsOwnProxyAfterAPortChange(t *testing.T) {
	b, f := cliBridge(t, `{}`)
	mustNoErr(t, func() error { _, err := b.SetSecurePairing(true); return err })
	mustNoErr(t, func() error { _, err := b.Enable(); return err })
	b.LAN = &portOverrideLAN{startPort: 54014, boundPortStale: 54014} // the next start falls back to another port
	mustNoErr(t, func() error { _, err := b.Regenerate(); return err })
	if f.cfg != proxyCfg("54014") {
		t.Fatalf("serve config = %s, want it re-pointed to 54014", f.cfg)
	}
	if got := loadState(t, b).ServeAppliedPort; got != 54014 {
		t.Errorf("ownership record = %d, want 54014", got)
	}
}

// A mode enabled by an earlier build has no ownership record; its live proxy
// points at the last bound port, which is still AO's.
func TestRestoreOnBootAdoptsLegacyProxyOnLastPort(t *testing.T) {
	b, f := cliBridge(t, proxyCfg("3011"))
	st := mobilebridge.State{Enabled: true, Password: "secret12", LastPort: 3011, SecurePairing: true, SecurePairingAutoEnabledV1: true}
	mustNoErr(t, func() error { return mobilebridge.Save(b.ConfigPath, st) })
	b.LAN = &portOverrideLAN{startPort: 54014, boundPortStale: 54014}
	mustNoErr(t, func() error { return b.RestoreOnBoot(st) })
	if f.cfg != proxyCfg("54014") {
		t.Fatalf("serve config = %s, want legacy proxy re-pointed to the bound port", f.cfg)
	}
}

func TestSecurePairingOffClearsOnlyAOsOwnRoute(t *testing.T) {
	t.Run("own route", func(t *testing.T) {
		b, f := cliBridge(t, `{}`)
		mustNoErr(t, func() error { _, err := b.SetSecurePairing(true); return err })
		mustNoErr(t, func() error { _, err := b.Enable(); return err })
		mustNoErr(t, func() error { _, err := b.SetSecurePairing(false); return err })
		if f.cfg != `{}` {
			t.Errorf("serve config = %s, want cleared", f.cfg)
		}
		if got := loadState(t, b).ServeAppliedPort; got != 0 {
			t.Errorf("ownership record = %d after clear", got)
		}
	})
	t.Run("config replaced by the user since", func(t *testing.T) {
		b, f := cliBridge(t, `{}`)
		mustNoErr(t, func() error { _, err := b.SetSecurePairing(true); return err })
		mustNoErr(t, func() error { _, err := b.Enable(); return err })
		f.cfg = staticCfg // the user took :443 over after AO applied
		f.writes = nil
		mustNoErr(t, func() error { _, err := b.SetSecurePairing(false); return err })
		if len(f.writes) != 0 || f.cfg != staticCfg {
			t.Errorf("removed a config AO does not own: writes=%v", f.writes)
		}
		if r := b.Status().SecurePairing.Reason; r == "clear_failed" {
			t.Error("reported clear_failed for a route that is no longer AO's")
		}
	})
	t.Run("disable and shutdown", func(t *testing.T) {
		b, f := cliBridge(t, `{}`)
		mustNoErr(t, func() error { _, err := b.SetSecurePairing(true); return err })
		mustNoErr(t, func() error { _, err := b.Enable(); return err })
		f.cfg = staticCfg
		f.writes = nil
		b.ShutdownServe()
		mustNoErr(t, b.Disable)
		if len(f.writes) != 0 || f.cfg != staticCfg {
			t.Errorf("shutdown/disable removed a foreign config: %v", f.writes)
		}
	})
}

// The phone triggers Status-equivalent work on every connect; the two CLI reads
// behind it are cached briefly, and any transition drops the cache.
func TestSecurePairingStatusIsCached(t *testing.T) {
	b, f := cliBridge(t, `{}`)
	now := time.Unix(1_000, 0)
	b.now = func() time.Time { return now }
	mustNoErr(t, func() error { _, err := b.SetSecurePairing(true); return err })
	mustNoErr(t, func() error { _, err := b.Enable(); return err })

	_ = b.AdvertisedEndpoints()
	f.reads = map[string]int{}
	for i := 0; i < 5; i++ {
		_ = b.AdvertisedEndpoints()
		_ = b.Status()
	}
	if f.reads["status --json"] != 0 || f.reads["serve status --json"] != 0 {
		t.Fatalf("cli reads within the TTL = %v, want none", f.reads)
	}
	now = now.Add(31 * time.Second)
	_ = b.AdvertisedEndpoints()
	if f.reads["status --json"] != 1 || f.reads["serve status --json"] != 1 {
		t.Errorf("cli reads after the TTL = %v, want one refresh", f.reads)
	}
	// A transition (apply/clear) invalidates immediately, no waiting for the TTL.
	mustNoErr(t, func() error { _, err := b.SetSecurePairing(false); return err })
	if b.Status().SecurePairing.Active {
		t.Error("stale cached Active after turning the mode off")
	}
}

func mustNoErr(t *testing.T, f func() error) {
	t.Helper()
	if err := f(); err != nil {
		t.Fatal(err)
	}
}

// A mode turned on by an earlier build has no ownership record. Turning it off
// must still remove that route: the owned ports come from the state as it was
// before the flag flipped.
func TestSecurePairingOffClearsALegacyRouteWithNoOwnershipRecord(t *testing.T) {
	b, f := cliBridge(t, proxyCfg("3011"))
	mustNoErr(t, func() error {
		return mobilebridge.Save(b.ConfigPath, mobilebridge.State{
			Enabled: true, Password: "secret12", LastPort: 3011, SecurePairing: true, SecurePairingAutoEnabledV1: true,
		})
	})
	mustNoErr(t, func() error { _, err := b.SetSecurePairing(false); return err })
	if f.cfg != `{}` {
		t.Fatalf("serve config = %s, want AO's legacy route removed", f.cfg)
	}
	if st := loadState(t, b); st.SecurePairing || st.ServeCleanupPending {
		t.Errorf("state = %+v, want off with no cleanup pending", st)
	}
}

// An older build can leave ServeCleanupPending set with the mode already off
// and no ownership record. Its route still points at the last bound port and is
// AO's to remove; clearing must not skip it, nor drop the pending flag.
func TestPendingCleanupOfALegacyRouteStillClearsIt(t *testing.T) {
	cases := []struct {
		name string
		run  func(b *BridgeService) error
	}{
		{"turning the mode off again", func(b *BridgeService) error { _, err := b.SetSecurePairing(false); return err }},
		{"disable", func(b *BridgeService) error { return b.Disable() }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, f := cliBridge(t, proxyCfg("3011"))
			mustNoErr(t, func() error {
				return mobilebridge.Save(b.ConfigPath, mobilebridge.State{
					Enabled: true, Password: "secret12", LastPort: 3011,
					SecurePairing: false, ServeCleanupPending: true, SecurePairingAutoEnabledV1: true,
				})
			})
			mustNoErr(t, func() error { return tc.run(b) })
			if f.cfg != `{}` {
				t.Fatalf("serve config = %s, want AO's legacy route removed", f.cfg)
			}
			if st := loadState(t, b); st.ServeCleanupPending {
				t.Errorf("state = %+v, cleanup still pending after a successful clear", st)
			}
		})
	}
}

// With neither flag set the last port is not evidence of ownership.
func TestOwnedServePortsNeedsAnOwnershipSignal(t *testing.T) {
	if got := ownedServePorts(mobilebridge.State{LastPort: 3011}); len(got) != 0 {
		t.Errorf("owned = %v with no record, mode off and nothing pending, want none", got)
	}
}
