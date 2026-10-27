// Package routeclass is the machine-checked class of every mutating route of the daemon (ED8).
//
// A route's class says who may call it: the guard in package httpd refuses a mutating request
// whose route is not in the table (deny by default), and a test walks the live router and the
// OpenAPI spec to keep the table and the routes identical in both directions, so a new route
// cannot ship unclassified and a removed route cannot leave a stale entry.
package routeclass

import (
	"sort"
	"strings"
)

// Class says who may call a route.
type Class string

const (
	// H means the person only. Needs a verified human capability.
	H Class = "H"
	// HP means the person only, but a verified agent or a terminal that asks is answered by filing a
	// pending request for the person to approve in the app (never by applying it).
	HP Class = "HP"
	// A means a verified agent or reviewer proof, or the person. The handler narrows it with the
	// route's predicate (self, owner, manager, coordinator, reviewer of that run).
	A Class = "A"
	// T means bound to one session by its own capability (browser, preview, hooks): a session proof
	// for itself, the capability headers those routes already check, or the person.
	T Class = "T"
	// O means open to any local caller (a state-free or self-authenticating route).
	O Class = "O"
	// LC means local control (shutdown, side-chat launch): loopback only, as before.
	LC Class = "LC"
	// OwnProof means authenticated by a credential in the request itself (the connection password or
	// account credential), not by caller identity.
	OwnProof Class = "OwnProof"
)

// Valid reports whether c is one of the defined classes.
func (c Class) Valid() bool {
	switch c {
	case H, HP, A, T, O, LC, OwnProof:
		return true
	}
	return false
}

// Observe says when an anonymous caller may use an A-class route while identity is only
// observed (the guard's observe mode). It is a fixed per-route flag, never derived from the
// caller.
type Observe string

const (
	// ObserveNone means an anonymous caller is refused in every mode (a predicate or authority route:
	// merge, review submit, kill, spawning an orchestrator).
	ObserveNone Observe = ""
	// ObserveOK means ed.7 needed no identity here; an anonymous caller passes in observe mode and is
	// counted.
	ObserveOK Observe = "ok"
	// ObserveKinds means anonymous passes only for the request kinds listed in Entry.Kinds (the body's
	// "kind"); every other kind refuses anonymous.
	ObserveKinds Observe = "kinds"
	// ObserveUntilCoordinatorProof means a coordinator-only route. An anonymous caller passes only
	// while NO coordinator session holds a proof; once one does, only the coordinator's own
	// identity passes.
	ObserveUntilCoordinatorProof Observe = "until-coordinator-proof"
)

// Entry is one classified route.
type Entry struct {
	Method string
	// Pattern is the chi route pattern, for example /api/v1/sessions/{sessionId}/send.
	Pattern string
	Class   Class
	Note    string
	// Observe, for an A-class route, says what an anonymous caller may do in observe mode.
	Observe Observe
	// Kinds lists the observe-allowed request kinds of an ObserveKinds route.
	Kinds []string
	// Future names the branch whose route this entry is waiting for. The entry is not required
	// to be mounted yet, and the build fails once it is, so the marker is removed on merge.
	Future string
}

// Entries returns a copy of the table.
func Entries() []Entry {
	out := make([]Entry, len(table))
	copy(out, table)
	return out
}

var index = func() map[string]Entry {
	m := make(map[string]Entry, len(table))
	for _, e := range table {
		m[key(e.Method, e.Pattern)] = e
	}
	return m
}()

func key(method, pattern string) string { return strings.ToUpper(method) + " " + pattern }

// Lookup returns the entry of a route pattern, or false when the route is unclassified.
func Lookup(method, pattern string) (Entry, bool) {
	e, ok := index[key(method, pattern)]
	return e, ok
}

// Keys lists every entry as "METHOD pattern", sorted.
func Keys() []string {
	out := make([]string, 0, len(table))
	for _, e := range table {
		out = append(out, key(e.Method, e.Pattern))
	}
	sort.Strings(out)
	return out
}

// IsWrite reports whether a request method changes state and therefore needs a class.
func IsWrite(method string) bool {
	switch strings.ToUpper(method) {
	case "GET", "HEAD", "OPTIONS":
		return false
	}
	return true
}
