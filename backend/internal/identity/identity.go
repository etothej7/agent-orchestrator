// Package identity answers "who is calling the daemon" without trusting anything the caller
// says about itself (ED8).
//
// Two identities exist. The person at the keyboard holds the human capability (package
// uicapability), which only the desktop app can mint. An agent holds a per-session proof: a
// random value the daemon issued when it launched the session, kept in that session's
// environment (AO_SESSION_PROOF) and sent by the ao CLI, the MCP server and the hooks as
// X-AO-Session-Proof. The daemon stores only the proof's hash. Everything else a request says
// about who sent it (a senderSessionId field, AO_SESSION_ID, a session id in a path) is a claim
// that must agree with the verified identity, never the identity itself.
//
// The goal is cooperative agents that cannot impersonate the person or each other through
// normal tooling. A same-user process that can read another process's environment can still
// read that process's proof; the human capability never travels through an environment.
package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

const (
	// HeaderProof carries a session proof on a request.
	HeaderProof = "X-AO-Session-Proof"
	// EnvProof is the environment variable a session's proof lives in. Its name contains none of
	// TOKEN, KEY, SECRET, AUTH or CRED: several harnesses drop environment variables whose names
	// look like secrets, and the proof must reach the ao subprocesses an agent runs.
	EnvProof = "AO_SESSION_PROOF"
	// EnvProofFile names a private file holding the proof, for sessions whose harness filters
	// the environment its commands see (a Codex reviewer's shell) or whose launch must not carry
	// the proof inline anywhere. The variable holds only a path, so it is safe in an argv.
	EnvProofFile = "AO_SESSION_PROOF_FILE"

	proofPrefix = "aop1."
	// proofBytes is the entropy of a proof: 256 bits, far above the 128-bit floor.
	proofBytes = 32
)

// Kind says what a request's caller proved to be.
type Kind string

const (
	// KindAnonymous proved nothing: no capability and no proof.
	KindAnonymous Kind = "anonymous"
	// KindHuman carried a valid human capability for this request.
	KindHuman Kind = "human"
	// KindAgent carried the proof of a live session.
	KindAgent Kind = "agent"
)

// Caller is the verified identity of one request.
type Caller struct {
	Kind        Kind
	Session     domain.SessionID
	SessionKind domain.SessionKind
	Project     domain.ProjectID
}

// Anonymous is the caller that proved nothing.
func Anonymous() Caller { return Caller{Kind: KindAnonymous} }

// Human is the caller that held a valid human capability.
func Human() Caller { return Caller{Kind: KindHuman} }

// IsHuman reports whether the caller is the person at the keyboard.
func (c Caller) IsHuman() bool { return c.Kind == KindHuman }

// IsAgent reports whether the caller is a verified session.
func (c Caller) IsAgent() bool { return c.Kind == KindAgent && c.Session != "" }

type callerKey struct{}

// WithCaller records the verified caller on a request context. Only the daemon's own
// identity middleware may call it.
func WithCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerKey{}, c)
}

// FromContext returns the verified caller of a request; a context nothing verified yields the
// anonymous caller, so an unmarked request can never be mistaken for anyone.
func FromContext(ctx context.Context) Caller {
	if c, ok := ctx.Value(callerKey{}).(Caller); ok {
		return c
	}
	return Anonymous()
}

// NewProof returns a fresh random proof: "aop1." plus 256 random bits, base64url. It is not
// derived from the session id or anything else.
func NewProof() (string, error) {
	buf := make([]byte, proofBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate session proof: %w", err)
	}
	return proofPrefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

// WellFormed reports whether s has the shape of a proof (prefix and 256 bits).
func WellFormed(s string) bool {
	rest, ok := strings.CutPrefix(s, proofPrefix)
	if !ok {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(rest)
	return err == nil && len(raw) == proofBytes
}

// Hash is the SHA-256 hex of a proof: the only form the daemon keeps.
func Hash(proof string) string {
	sum := sha256.Sum256([]byte(proof))
	return hex.EncodeToString(sum[:])
}

// maxProofFileBytes bounds what a proof file may hold: a proof is 48 characters.
const maxProofFileBytes = 256

// ProofFromEnv returns the identity proof of the current process: AO_SESSION_PROOF when set,
// otherwise the contents of the private file AO_SESSION_PROOF_FILE names. It returns "" when
// there is none or the file is not a plain, absolute-path, owner-only regular file holding a
// well-formed proof: a proof is never taken from anywhere that others could have written or
// swapped.
func ProofFromEnv(getenv func(string) string) string {
	if proof := strings.TrimSpace(getenv(EnvProof)); proof != "" {
		return proof
	}
	path := strings.TrimSpace(getenv(EnvProofFile))
	if path == "" || !filepath.IsAbs(path) {
		return ""
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > maxProofFileBytes {
		return ""
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	proof := strings.TrimSpace(string(raw))
	if !WellFormed(proof) {
		return ""
	}
	return proof
}

// Errors a verification can end in; each one is a refusal.
var (
	ErrMissing = errors.New("session proof is missing")
	ErrInvalid = errors.New("session proof is invalid")
	// ErrSessionEnded means the proof was valid but its session has ended: a terminated
	// session's proof stops verifying.
	ErrSessionEnded = errors.New("session proof belongs to a session that has ended")
)

// Store is what the daemon keeps about proofs.
type Store interface {
	SetSessionProofHash(ctx context.Context, id domain.SessionID, hash string, now time.Time) error
	SessionIdentityByProofHash(ctx context.Context, hash string) (id domain.SessionID, storedHash string, found bool, err error)
	DeleteSessionProof(ctx context.Context, id domain.SessionID) error
}

// SessionReader reads the session a proof was issued to.
type SessionReader interface {
	GetSession(ctx context.Context, id domain.SessionID) (domain.SessionRecord, bool, error)
}

// Issuer issues and revokes session proofs.
type Issuer struct {
	store Store
	now   func() time.Time
}

// NewIssuer builds an Issuer; now may be nil for the real clock.
func NewIssuer(store Store, now func() time.Time) *Issuer {
	if now == nil {
		now = time.Now
	}
	return &Issuer{store: store, now: now}
}

// Issue gives the session a new proof and returns it. It is the ONLY moment the plain proof
// exists on the daemon's side: the caller must put it into the session's environment and
// drop it. Issuing again replaces the previous proof, which stops verifying.
func (i *Issuer) Issue(ctx context.Context, id domain.SessionID) (string, error) {
	proof, err := NewProof()
	if err != nil {
		return "", err
	}
	if err := i.store.SetSessionProofHash(ctx, id, Hash(proof), i.now()); err != nil {
		return "", err
	}
	return proof, nil
}

// Revoke removes a session's proof.
func (i *Issuer) Revoke(ctx context.Context, id domain.SessionID) error {
	return i.store.DeleteSessionProof(ctx, id)
}

// Resolver turns a presented proof into the session it identifies.
type Resolver struct {
	store    Store
	sessions SessionReader
}

// NewResolver builds a Resolver.
func NewResolver(store Store, sessions SessionReader) *Resolver {
	return &Resolver{store: store, sessions: sessions}
}

// Resolve verifies a proof: well formed, issued to a session, constant-time equal to the stored
// hash, and that session has not ended. The caller it returns is derived from the daemon's
// own session record, never from the request.
func (r *Resolver) Resolve(ctx context.Context, proof string) (Caller, error) {
	proof = strings.TrimSpace(proof)
	if proof == "" {
		return Anonymous(), ErrMissing
	}
	if !WellFormed(proof) {
		return Anonymous(), ErrInvalid
	}
	want := Hash(proof)
	id, stored, found, err := r.store.SessionIdentityByProofHash(ctx, want)
	if err != nil {
		return Anonymous(), fmt.Errorf("resolve session proof: %w", err)
	}
	if !found || subtle.ConstantTimeCompare([]byte(stored), []byte(want)) != 1 {
		return Anonymous(), ErrInvalid
	}
	rec, ok, err := r.sessions.GetSession(ctx, id)
	if err != nil {
		return Anonymous(), fmt.Errorf("resolve session proof: %w", err)
	}
	if !ok || rec.IsTerminated {
		return Anonymous(), ErrSessionEnded
	}
	return Caller{Kind: KindAgent, Session: rec.ID, SessionKind: rec.Kind, Project: rec.ProjectID}, nil
}
