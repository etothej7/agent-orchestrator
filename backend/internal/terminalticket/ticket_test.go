package terminalticket

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestATicketWorksOnceWithinItsLifetime(t *testing.T) {
	now := time.Unix(1000, 0)
	s := New(func() time.Time { return now })
	ticket, ttl, err := s.Issue()
	if err != nil || ttl != TTL || len(ticket) < 40 {
		t.Fatalf("issue = %q, %v, %v", ticket, ttl, err)
	}
	if !s.Redeem(ticket) {
		t.Fatal("a fresh ticket was refused")
	}
	if s.Redeem(ticket) {
		t.Fatal("a ticket worked twice")
	}
}

func TestATicketExpiresAndAnUnknownOrMalformedOneNeverWorks(t *testing.T) {
	now := time.Unix(1000, 0)
	s := New(func() time.Time { return now })
	ticket, _, _ := s.Issue()
	now = now.Add(TTL)
	if s.Redeem(ticket) {
		t.Fatal("an expired ticket worked")
	}
	for _, bad := range []string{"", "nope", strings.Repeat("a", 129), ticket + "x"} {
		if s.Redeem(bad) {
			t.Errorf("ticket %q worked", bad)
		}
	}
	other := New(func() time.Time { return now })
	fresh, _, _ := s.Issue()
	if other.Redeem(fresh) {
		t.Fatal("a ticket worked on a store that did not issue it")
	}
}

func TestTicketsAreRandomAndTheStoreKeepsOnlyTheirHashes(t *testing.T) {
	s := New(nil)
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		ticket, _, err := s.Issue()
		if err != nil || seen[ticket] {
			t.Fatalf("ticket %d = %q, %v (duplicate or error)", i, ticket, err)
		}
		seen[ticket] = true
		if _, kept := s.pending[ticket]; kept {
			t.Fatal("the store keeps the ticket itself, not its hash")
		}
	}
}

func TestTheStoreRefusesToGrowWithoutBoundAndRecoversWhenTicketsExpire(t *testing.T) {
	now := time.Unix(1000, 0)
	s := New(func() time.Time { return now })
	for i := 0; i < MaxOutstanding; i++ {
		if _, _, err := s.Issue(); err != nil {
			t.Fatalf("ticket %d: %v", i, err)
		}
	}
	if _, _, err := s.Issue(); !errors.Is(err, ErrTooMany) {
		t.Fatalf("past the cap: %v", err)
	}
	now = now.Add(TTL + time.Second)
	if _, _, err := s.Issue(); err != nil {
		t.Fatalf("after the old tickets expired: %v", err)
	}
	if len(s.pending) != 1 {
		t.Fatalf("expired tickets were kept: %d", len(s.pending))
	}
}
