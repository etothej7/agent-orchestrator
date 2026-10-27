package mobilebridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Serve drives the tailnet-only HTTPS proxy that fronts the Connect Mobile
// bridge, so phones reach it over TLS with Tailscale's certificate instead of
// plaintext HTTP. Tailnet-only: this is `serve`, never `funnel`, so nothing is
// exposed publicly.
type Serve struct {
	// Run is the CLI runner; nil means the real tailscale binary.
	Run TailscaleRunner
}

// NewServe returns a Serve backed by the real tailscale CLI.
func NewServe() *Serve { return &Serve{} }

func (s *Serve) run(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, tailscaleTimeout)
	defer cancel()
	if s.Run != nil {
		return s.Run(ctx, args...)
	}
	return execTailscale(ctx, args...)
}

// ErrForeignServe means tailnet :443 holds a config AO did not apply, or that
// AO cannot prove is its own. It is never overwritten or removed.
var ErrForeignServe = errors.New("tailnet :443 is configured by something other than AO")

// ErrFunnelExposed means Tailscale Funnel is switched on for :443, which would
// publish the bridge on the public internet. AO only ever uses tailnet-only
// `serve`, so it refuses rather than ride on that exposure.
var ErrFunnelExposed = errors.New("tailnet :443 is exposed publicly through funnel")

// Apply points the tailnet's HTTPS :443 proxy at the given local bridge port.
// Idempotent, and safe to call on every listener start — which is exactly how
// the caller avoids a proxy left on a dead port.
//
// `tailscale serve --https=443` silently replaces whatever is on :443, and that
// config is node-global, so Apply first reads it and proceeds only when :443 is
// verifiably empty or is the single loopback proxy AO itself applied earlier —
// recognised by its target being one of ownedPorts (the ports AO's ownership
// record says it applied). Anything else, an unreadable config, or an enabled
// funnel flag is refused with no change made.
func (s *Serve) Apply(ctx context.Context, bridgePort int, ownedPorts ...int) error {
	cfg, err := s.inspect(ctx)
	if err != nil {
		return err
	}
	if cfg.funnel {
		return ErrFunnelExposed
	}
	if cfg.inUse && !cfg.ownedBy(ownedPorts) {
		return ErrForeignServe
	}
	target := fmt.Sprintf("http://127.0.0.1:%d", bridgePort)
	if _, err := s.run(ctx, "serve", "--bg", "--https=443", target); err != nil {
		return fmt.Errorf("tailscale serve --https=443 %s: %w", target, err)
	}
	return nil
}

// Clear removes the :443 proxy, but only while it is still the one AO applied
// (its target is in ownedPorts). An empty :443 needs nothing; a config that is
// someone else's is left strictly alone and reported as success, because AO's
// own route is already gone. An unreadable config is an error: Clear must not
// guess, and the caller keeps its cleanup pending.
func (s *Serve) Clear(ctx context.Context, ownedPorts ...int) error {
	cfg, err := s.inspect(ctx)
	if err != nil {
		return err
	}
	if !cfg.inUse || !cfg.ownedBy(ownedPorts) {
		return nil
	}
	if _, err := s.run(ctx, "serve", "--https=443", "off"); err != nil {
		return fmt.Errorf("tailscale serve --https=443 off: %w", err)
	}
	return nil
}

func containsPort(ports []int, p int) bool {
	for _, q := range ports {
		if q > 0 && q == p {
			return true
		}
	}
	return false
}

// serve443 classifies what tailnet :443 currently holds.
type serve443 struct {
	// inUse: anything at all is configured on :443 (HTTPS site, TCP forward, or
	// a web handler).
	inUse bool
	// funnel: an AllowFunnel flag is true for a :443 site.
	funnel bool
	// proxyPort is non-zero only when :443 is exactly the shape AO applies: one
	// HTTPS site with a single "/" handler proxying to loopback. Any other
	// content (static text/path, redirect, extra handlers, TCP forward, a
	// non-loopback proxy) leaves it zero, so it can never read as AO's.
	proxyPort int
}

// ownedBy reports whether :443 is AO's single loopback proxy to one of ports.
func (c serve443) ownedBy(ports []int) bool {
	return c.proxyPort > 0 && containsPort(ports, c.proxyPort)
}

// inspect reads `tailscale serve status --json`. A failed call or unparseable
// output is an error — callers must treat it as "cannot prove it is empty".
func (s *Serve) inspect(ctx context.Context) (serve443, error) {
	out, err := s.run(ctx, "serve", "status", "--json")
	if err != nil {
		return serve443{}, fmt.Errorf("tailscale serve status: %w", err)
	}
	if strings.TrimSpace(string(out)) == "" {
		return serve443{}, nil
	}
	var parsed struct {
		TCP map[string]struct {
			HTTPS        bool   `json:"HTTPS"`
			TCPForward   string `json:"TCPForward"`
			TerminateTLS string `json:"TerminateTLS"`
		} `json:"TCP"`
		Web map[string]struct {
			Handlers map[string]struct {
				Proxy    string `json:"Proxy"`
				Path     string `json:"Path"`
				Text     string `json:"Text"`
				Redirect string `json:"Redirect"`
			} `json:"Handlers"`
		} `json:"Web"`
		AllowFunnel map[string]bool `json:"AllowFunnel"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return serve443{}, fmt.Errorf("parse tailscale serve status: %w", err)
	}
	var cfg serve443
	tcp, hasTCP := parsed.TCP["443"]
	cfg.inUse = hasTCP
	for key, on := range parsed.AllowFunnel {
		if on && strings.HasSuffix(key, ":443") {
			cfg.funnel = true
		}
	}
	var sites []string
	for key := range parsed.Web {
		if strings.HasSuffix(key, ":443") {
			sites = append(sites, key)
		}
	}
	if len(sites) > 0 {
		cfg.inUse = true
	}
	if len(sites) != 1 || !hasTCP || !tcp.HTTPS || tcp.TCPForward != "" || tcp.TerminateTLS != "" {
		return cfg, nil
	}
	handlers := parsed.Web[sites[0]].Handlers
	h, ok := handlers["/"]
	if len(handlers) != 1 || !ok || h.Path != "" || h.Text != "" || h.Redirect != "" {
		return cfg, nil
	}
	if u, perr := url.Parse(h.Proxy); perr == nil && u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost") {
		if port, aerr := strconv.Atoi(u.Port()); aerr == nil && port > 0 {
			cfg.proxyPort = port
		}
	}
	return cfg, nil
}

// Target reports the local port :443 currently proxies to, or 0 when unset or
// undeterminable. Callers compare it against the live bridge port to detect a
// stale config left by a crash or an out-of-band edit.
func (s *Serve) Target(ctx context.Context) int {
	out, err := s.run(ctx, "serve", "status", "--json")
	if err != nil {
		return 0
	}
	var parsed struct {
		Web map[string]struct {
			Handlers map[string]struct{ Proxy string } `json:"Handlers"`
		} `json:"Web"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return 0
	}
	// parsed.Web is keyed "hostname:port". A user may also run `tailscale serve`
	// for something unrelated on another port (e.g. --https=8443); skip any site
	// not on :443 so its handler is never mistaken for AO's proxy target — Go
	// map iteration order would otherwise make that pick random.
	for key, site := range parsed.Web {
		if !strings.HasSuffix(key, ":443") {
			continue
		}
		for _, h := range site.Handlers {
			u, err := url.Parse(h.Proxy)
			if err != nil {
				continue
			}
			if port, err := strconv.Atoi(u.Port()); err == nil {
				return port
			}
		}
	}
	return 0
}

// HTTPSInUse reports whether anything is already configured on tailnet :443 —
// an HTTPS proxy, a TCP forward, or a web site on that port. It is a read-only
// probe. An error means the config could not be read, in which case the caller
// cannot prove :443 is free and must treat it as taken.
func (s *Serve) HTTPSInUse(ctx context.Context) (bool, error) {
	cfg, err := s.inspect(ctx)
	if err != nil {
		return false, err
	}
	return cfg.inUse || cfg.funnel, nil
}
