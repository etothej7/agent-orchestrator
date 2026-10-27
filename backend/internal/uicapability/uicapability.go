// Package uicapability proves that a request came from AO's own desktop UI.
//
// The Electron main process mints one random secret per app launch and hands it to
// the daemon it spawns over that daemon's private stdin pipe (the same handoff the
// browser runtime uses): never an environment variable, a file or a command line,
// which a same-user agent could read. The daemon keeps it in memory only. A request
// that must come from the person at the keyboard carries a capability derived from
// that secret: an HMAC over a purpose, a subject and a timestamp. The secret itself
// never travels, and a capability cannot be used for another purpose, another
// session, or later than its window.
//
// An agent process (same Unix user) has neither the secret nor a way to ask the
// main process to sign for it: the signer is reachable only from the trusted shell
// window's preload bridge. A daemon that was not started by the app (headless) holds
// a random secret nobody knows, so nothing can be lifted there.
package uicapability

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Header carries the capability on a request.
const Header = "X-AO-UI-Capability"

// PurposeHumanWrite marks a write the person at the keyboard made through AO's UI. The
// capability is bound to ONE request: its subject is the HTTP method and path, so a
// captured header cannot be replayed against another endpoint or session.
const PurposeHumanWrite = "human-write"

// RequestSubject is the subject a human-write capability is bound to.
func RequestSubject(method, path string) string { return strings.ToUpper(method) + " " + path }

// Window is how long a capability stays valid after it was signed, in either
// direction, so a captured header cannot be replayed later.
const Window = 60 * time.Second

const version = "v1"

// Errors a verification can end in. All of them are a refusal.
var (
	ErrMissing = errors.New("ui capability is missing")
	ErrInvalid = errors.New("ui capability is invalid")
	ErrExpired = errors.New("ui capability is expired")
)

// Verifier checks capabilities against the per-run secret. The zero value (and a
// Verifier built from an empty secret) refuses everything.
type Verifier struct{ secret []byte }

// New builds a verifier from the per-run secret.
func New(secret string) *Verifier {
	if secret == "" {
		return &Verifier{}
	}
	return &Verifier{secret: []byte(secret)}
}

func mac(secret []byte, purpose, subject string, unix int64) string {
	h := hmac.New(sha256.New, secret)
	fmt.Fprintf(h, "ao-ui-capability:%s:%s:%s:%d", version, purpose, subject, unix)
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

// Sign produces a capability. The daemon never signs; this is for tests and for the
// parity test of the Electron signer.
func Sign(secret, purpose, subject string, now time.Time) string {
	unix := now.Unix()
	return fmt.Sprintf("%s.%d.%s", version, unix, mac([]byte(secret), purpose, subject, unix))
}

// VerifyRequest accepts a human-write capability for exactly this method and path.
func (v *Verifier) VerifyRequest(header, method, path string, now time.Time) error {
	return v.Verify(header, PurposeHumanWrite, RequestSubject(method, path), now)
}

// Verify accepts a capability only if it was signed with this run's secret for this
// purpose and subject, within Window of now.
func (v *Verifier) Verify(header, purpose, subject string, now time.Time) error {
	if v == nil || len(v.secret) == 0 {
		return ErrInvalid
	}
	header = strings.TrimSpace(header)
	if header == "" {
		return ErrMissing
	}
	parts := strings.Split(header, ".")
	if len(parts) != 3 || parts[0] != version {
		return ErrInvalid
	}
	unix, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return ErrInvalid
	}
	// The MAC is checked before the clock so an unauthentic value never learns whether
	// its timestamp was plausible.
	if !hmac.Equal([]byte(parts[2]), []byte(mac(v.secret, purpose, subject, unix))) {
		return ErrInvalid
	}
	if d := now.Sub(time.Unix(unix, 0)); d > Window || d < -Window {
		return ErrExpired
	}
	return nil
}
