// Package terminalticket issues the single-use tickets that let the app's own terminals attach to /mux.
//
// A terminal WebSocket cannot carry a per-request capability (the page cannot sign an upgrade), and attaching
// one lets its holder type into the person's shells and agent terminals. So the app asks for a ticket with an
// ordinary signed POST, which only the person's app can make, and offers it on the upgrade. A ticket is random,
// lives seconds, and works once; only its hash is kept.
package terminalticket

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

const (
	// TTL is how long a ticket may wait for its upgrade.
	TTL = 30 * time.Second
	// MaxOutstanding caps unredeemed tickets so a loop of requests cannot grow memory.
	MaxOutstanding = 256
	// SubprotocolPrefix marks the ticket among the subprotocols the client offers.
	SubprotocolPrefix = "ao.ticket."
	// Subprotocol is the one the server selects, so the ticket itself is never echoed back.
	Subprotocol = "ao.mux"
)

// ErrTooMany means there are already too many unredeemed tickets.
var ErrTooMany = errors.New("too many outstanding terminal tickets")

// Store holds unredeemed tickets by hash.
type Store struct {
	mu      sync.Mutex
	now     func() time.Time
	pending map[string]time.Time
}

// New builds a Store. A nil clock uses time.Now.
func New(now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{now: now, pending: map[string]time.Time{}}
}

func hashOf(ticket string) string {
	sum := sha256.Sum256([]byte(ticket))
	return hex.EncodeToString(sum[:])
}

// Issue mints a ticket valid for TTL.
func (s *Store) Issue() (ticket string, expiresIn time.Duration, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.purgeLocked(now)
	if len(s.pending) >= MaxOutstanding {
		return "", 0, ErrTooMany
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", 0, err
	}
	ticket = base64.RawURLEncoding.EncodeToString(raw)
	s.pending[hashOf(ticket)] = now.Add(TTL)
	return ticket, TTL, nil
}

// Redeem consumes a ticket: true once, for an unexpired ticket this store issued, never again.
func (s *Store) Redeem(ticket string) bool {
	if ticket == "" || len(ticket) > 128 {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := hashOf(ticket)
	expires, ok := s.pending[key]
	if !ok {
		return false
	}
	delete(s.pending, key)
	return s.now().Before(expires)
}

func (s *Store) purgeLocked(now time.Time) {
	for key, expires := range s.pending {
		if !now.Before(expires) {
			delete(s.pending, key)
		}
	}
}
