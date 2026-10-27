package apispec_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	yaml "gopkg.in/yaml.v3"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/terminal"
)

// ed.8 caller-identity coverage: the route-class table is only as strong as the list of
// routes it is checked against. This walks the LIVE router with EVERY conditionally
// mounted group switched on (mobile bridge and remote host, device roster, telemetry,
// shutdown, browser live) so that no write route, in particular the password-bearing
// /mobile/* group, is invisible to the unclassified-route build guard.

type nopSink struct{}

func (nopSink) Emit(context.Context, ports.TelemetryEvent) {}
func (nopSink) Close(context.Context) error                { return nil }

type nopIdentity struct{ ports.TelemetryIdentityStore }

// muxSource only has to exist so the terminal mux (a write-capable WebSocket) is mounted.
type muxSource struct{ ports.Attacher }

func (muxSource) IsAlive(context.Context, ports.RuntimeHandle) (bool, error) { return true, nil }

// liveWriteRoutes returns every non-GET/HEAD/OPTIONS route of the fully mounted router,
// plus the GET WebSocket upgrade at /mux, keyed "METHOD path".
func liveWriteRoutes(t *testing.T) []string {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	deps := httpd.APIDeps{
		Mobile:            &controllers.MobileController{},
		DeviceRoster:      stubDeviceRoster{},
		DeviceLive:        stubDeviceLive{},
		Telemetry:         nopSink{},
		TelemetryIdentity: nopIdentity{},
	}
	mgr := terminal.NewManager(muxSource{}, nil, log)
	t.Cleanup(mgr.Close)
	router := httpd.NewRouterWithControl(config.Config{}, log, mgr, deps, httpd.ControlDeps{RequestShutdown: func() {}})
	seen := map[string]bool{}
	if err := chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		switch method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			// the terminal mux upgrades a GET into a write-capable WebSocket
			if route == "/mux" {
				seen["GET "+route] = true
			}
		default:
			seen[method+" "+route] = true
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(seen))
	for r := range seen {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

func TestLiveRouterWriteRoutesCoverEveryOpenAPIWriteOperation(t *testing.T) {
	live := liveWriteRoutes(t)
	if p := os.Getenv("AO_ROUTE_DUMP"); p != "" {
		_ = os.WriteFile(p, []byte(strings.Join(live, "\n")+"\n"), 0o600)
	}
	mounted := map[string]bool{}
	for _, r := range live {
		mounted[r] = true
	}
	// every non-GET OpenAPI operation must be a mounted live route: a group that is
	// not switched on here would silently drop out of the class table's check.
	spec := apispec.Default()
	var specWrites []string
	var doc struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(spec.YAML(), &doc); err != nil {
		t.Fatal(err)
	}
	for path, ops := range doc.Paths {
		for method := range ops {
			m := strings.ToUpper(method)
			if m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions || m == "PARAMETERS" || m == "SERVERS" {
				continue
			}
			specWrites = append(specWrites, m+" "+path)
		}
	}
	sort.Strings(specWrites)
	var missing []string
	for _, op := range specWrites {
		if !mounted[op] {
			missing = append(missing, op)
		}
	}
	if len(missing) != 0 {
		t.Fatalf("%d OpenAPI write operations are not mounted by the coverage walk (a conditionally mounted group is switched off): %v", len(missing), missing)
	}
	// floors: the walked count must never drop below what ed.7 ships. 206 OpenAPI write
	// operations, plus /shutdown, the three /internal/telemetry posts and the /mux upgrade.
	const specWriteFloor, liveFloor = 206, 211
	if len(specWrites) < specWriteFloor {
		t.Fatalf("OpenAPI write operations = %d, want at least %d", len(specWrites), specWriteFloor)
	}
	if len(live) < liveFloor {
		t.Fatalf("live write routes = %d, want at least %d: %v", len(live), liveFloor, live)
	}
	for _, must := range []string{"POST /shutdown", "GET /mux", "POST /api/v1/mobile/enable", "POST /api/v1/mobile/regenerate", "POST /api/v1/remote-host/account-token", "POST /internal/telemetry/cli-invoked"} {
		if !mounted[must] {
			t.Errorf("live router does not expose %q to the coverage walk", must)
		}
	}
}
