//go:build !darwin

package procidentity

import "errors"

// NewSystem is unavailable off macOS: the bridge is macOS-only for now. Callers treat the error as
// "no process lookup" and rely on the observe-allowed routes and re-proof instead.
func NewSystem() (ProcTable, PeerLookup, error) {
	return nil, nil, errors.New("process lookup is only implemented on macOS")
}
