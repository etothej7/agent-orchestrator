package mobilebridge

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"time"

	aoprocess "github.com/aoagents/agent-orchestrator/backend/internal/process"
)

// tailscaleTimeout bounds every CLI call. The Connect Mobile status endpoint is
// polled by the modal, so a wedged tailscaled must never stall it.
const tailscaleTimeout = 3 * time.Second

// TailscaleRunner runs the tailscale CLI and returns its stdout. Injected so
// tests never invoke the real binary.
type TailscaleRunner func(ctx context.Context, args ...string) ([]byte, error)

// execTailscale is the production TailscaleRunner.
func execTailscale(ctx context.Context, args ...string) ([]byte, error) {
	return aoprocess.CommandContext(ctx, tailscaleBinary(), args...).Output()
}

// magicDNSName is the shape of a node's MagicDNS name: DNS labels under
// <tailnet>.ts.net. It rejects ports, paths, userinfo and whitespace, since the
// name is later embedded in an https URL and a pairing QR.
var magicDNSName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+\.ts\.net$`)

// TailscaleInfo is what the local daemon can tell us about this node.
type TailscaleInfo struct {
	// Name is the MagicDNS name with no trailing dot, or "" when unavailable.
	Name string
	// CertsEnabled reports whether the tailnet can issue HTTPS certificates,
	// which `tailscale serve --https` requires.
	CertsEnabled bool
}

// QueryTailscale reports this node's MagicDNS name and certificate
// availability. Every failure — missing binary, non-zero exit, malformed
// output, a name that is not a MagicDNS name — yields the zero value; callers treat an empty Name as
// "secure pairing unavailable" rather than as an error to surface.
func QueryTailscale(ctx context.Context) TailscaleInfo {
	return queryTailscale(ctx, execTailscale)
}

// QueryTailscaleVia is QueryTailscale with an injected CLI runner.
func QueryTailscaleVia(ctx context.Context, run TailscaleRunner) TailscaleInfo {
	return queryTailscale(ctx, run)
}

func queryTailscale(ctx context.Context, run TailscaleRunner) TailscaleInfo {
	ctx, cancel := context.WithTimeout(ctx, tailscaleTimeout)
	defer cancel()
	out, err := run(ctx, "status", "--json")
	if err != nil {
		return TailscaleInfo{}
	}
	var parsed struct {
		Self        *struct{ DNSName string } `json:"Self"`
		CertDomains []string                  `json:"CertDomains"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil || parsed.Self == nil {
		return TailscaleInfo{}
	}
	name := strings.ToLower(strings.TrimSuffix(parsed.Self.DNSName, "."))
	if !magicDNSName.MatchString(name) {
		return TailscaleInfo{}
	}
	// Certificates must be available for THIS node's name, not merely exist for
	// some other domain on the tailnet.
	certs := false
	for _, d := range parsed.CertDomains {
		if strings.ToLower(strings.TrimSuffix(d, ".")) == name {
			certs = true
		}
	}
	return TailscaleInfo{Name: name, CertsEnabled: certs}
}
