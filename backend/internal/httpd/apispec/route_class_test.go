package apispec_test

import (
	"net/http"
	"sort"
	"strings"
	"testing"

	yaml "gopkg.in/yaml.v3"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/routeclass"
)

// The route-class table is the daemon's list of who may call every mutating route. It must be
// exactly the set of routes that exist, in both directions: a route without a class would be
// unguarded (so the guard denies it, and this test fails the build first), and an entry without
// a route is stale. Routes come from two independent sources that must agree with the table:
// the live router (every conditional group mounted) and the OpenAPI spec.
func TestRouteClassTableIsExactlyTheLiveWriteRoutes(t *testing.T) {
	live := map[string]bool{}
	for _, r := range liveWriteRoutes(t) {
		live[r] = true
	}
	var doc struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(apispec.Default().YAML(), &doc); err != nil {
		t.Fatal(err)
	}
	spec := map[string]bool{}
	for path, ops := range doc.Paths {
		for method := range ops {
			m := strings.ToUpper(method)
			if m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions || m == "PARAMETERS" || m == "SERVERS" {
				continue
			}
			spec[m+" "+path] = true
		}
	}

	table := map[string]routeclass.Entry{}
	for _, e := range routeclass.Entries() {
		key := e.Method + " " + e.Pattern
		if _, dup := table[key]; dup {
			t.Errorf("%s is classified twice", key)
		}
		if !e.Class.Valid() {
			t.Errorf("%s has an unknown class %q", key, e.Class)
		}
		table[key] = e
	}

	var unclassified, stale, futureMounted, invalidFuture []string
	for r := range live {
		if _, ok := table[r]; !ok {
			unclassified = append(unclassified, r)
		}
	}
	for r := range spec {
		if _, ok := table[r]; !ok {
			unclassified = append(unclassified, r+" (OpenAPI)")
		}
	}
	for key, e := range table {
		switch {
		case e.Future != "" && live[key]:
			futureMounted = append(futureMounted, key+" ("+e.Future+")")
		case e.Future != "":
			// waiting for another branch's route
		case !live[key]:
			stale = append(stale, key)
		}
		if e.Future != "" && e.Class != routeclass.H && e.Class != routeclass.HP {
			invalidFuture = append(invalidFuture, key)
		}
	}
	for _, s := range [][]string{unclassified, stale, futureMounted, invalidFuture} {
		sort.Strings(s)
	}
	if len(unclassified) != 0 {
		t.Errorf("%d mutating routes have no class in routeclass (add them; the guard denies unclassified routes): %v", len(unclassified), unclassified)
	}
	if len(stale) != 0 {
		t.Errorf("%d class entries match no live route (remove them): %v", len(stale), stale)
	}
	if len(futureMounted) != 0 {
		t.Errorf("entries waiting for another branch's route are now mounted; remove their Future marker: %v", futureMounted)
	}
	if len(invalidFuture) != 0 {
		t.Errorf("a not-yet-mounted route must be human-only (H or HP) until it lands: %v", invalidFuture)
	}
}

// The classes the design fixed by name must not drift in a regeneration.
func TestRouteClassPinnedRows(t *testing.T) {
	pinned := map[string]routeclass.Class{
		"POST /api/v1/terminal-tickets":                           routeclass.H, // agents never get a /mux attach ticket
		"GET /mux":                                                routeclass.H, // the terminal WebSocket upgrade (R1)
		"POST /api/v1/mobile/enable":                              routeclass.H,
		"POST /api/v1/mobile/regenerate":                          routeclass.H, // returns the pairing password
		"POST /api/v1/mobile/disable":                             routeclass.H, // coordinator ruling: no agent exception
		"POST /api/v1/remote-host/account-token":                  routeclass.OwnProof,
		"PUT /api/v1/github/pat":                                  routeclass.H,
		"POST /shutdown":                                          routeclass.LC,
		"POST /api/v1/sessions/{sessionId}/conversation/messages": routeclass.H, // Ed's composer; agents use send / steer-or-send (A)
		"POST /api/v1/sessions/{sessionId}/send":                  routeclass.A,
	}
	for key, want := range pinned {
		method, pattern, _ := strings.Cut(key, " ")
		got, ok := routeclass.Lookup(method, pattern)
		if !ok {
			t.Errorf("%s is not classified", key)
			continue
		}
		if got.Class != want {
			t.Errorf("%s is class %s, want %s", key, got.Class, want)
		}
	}
}
