// Package uicapability proves that a request came from AO's own desktop UI.
//
// The Electron main process mints one random key per app launch and hands it to
// the daemon it spawns over that daemon's private stdin pipe (the same handoff the
// browser runtime uses; the key is the second line): never an environment variable, a file or a command line,
// which a same-user agent could read. The daemon keeps it in memory only. A request
// that must come from the person at the keyboard carries a capability derived from
// that secret: an HMAC over a purpose, a subject and a timestamp. The secret itself
// never travels, and a capability cannot be used for another purpose, another
// session, or later than its window.
//
// An agent process (same Unix user) has neither the key nor a way to get the main
// process to sign for it: main signs only requests the trusted shell window sends to
// the daemon. A daemon that was not started by the app (headless) has no key, so no
// request to it is ever human.
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

// StdinEnv tells an app-spawned daemon that the second line of its private stdin
// handoff is the human-capability key (the first line is the browser-runtime token).
// The variable carries only this marker, never the key.
const StdinEnv = "AO_UI_CAPABILITY_STDIN"

// MinKeyBytes is the shortest key the daemon accepts (base64url text of 32 random bytes
// is 43 characters); a shorter handoff is refused rather than trusted.
const MinKeyBytes = 32

// ValidKey reports whether a handed-over key is long enough and base64url text.
func ValidKey(key string) bool {
	if len(key) < MinKeyBytes {
		return false
	}
	for _, c := range key {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// probeLabel keeps a possession proof from ever being usable as (or confused with) a request capability.
const probeLabel = "ao-ui-capability-probe:" + version + ":"

// ValidNonce reports whether a probe nonce is long enough to be unguessable and plain base64url text.
func ValidNonce(nonce string) bool {
	if len(nonce) < 16 || len(nonce) > 128 {
		return false
	}
	for _, c := range nonce {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// ProbeProof is what a daemon that holds the run's key answers to a nonce: an HMAC under its own label.
// The app sends a fresh nonce to a daemon address before it ever signs a request for it, so a process that
// merely listens on the port the app was told about (a forged run file) cannot be signed for. Mirror of the
// Electron main process; pinned by the shared vectors file.
func ProbeProof(key, nonce string) string {
	h := hmac.New(sha256.New, []byte(key))
	_, _ = h.Write([]byte(probeLabel + nonce))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

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
	_, _ = fmt.Fprintf(h, "ao-ui-capability:%s:%s:%s:%d", version, purpose, subject, unix)
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

// Enabled reports whether this verifier can verify anything: false for nil and for one without a key (a daemon the
// app did not start), where nothing is ever human and nothing human-gated can be enforced.
func (v *Verifier) Enabled() bool { return v != nil && len(v.secret) > 0 }

// Probe answers a possession probe for nonce. ok is false for a verifier without a key (a daemon the app
// did not start, which therefore must never be signed for) or a nonce that is not well formed.
func (v *Verifier) Probe(nonce string) (proof string, ok bool) {
	if v == nil || len(v.secret) == 0 || !ValidNonce(nonce) {
		return "", false
	}
	return ProbeProof(string(v.secret), nonce), true
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
	// Only the canonical spelling: "+123" and "0123" parse to the same number but are a different header.
	if err != nil || strconv.FormatInt(unix, 10) != parts[1] {
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
