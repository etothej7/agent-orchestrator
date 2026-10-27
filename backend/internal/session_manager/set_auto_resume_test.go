package sessionmanager

import (
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func autoResumeAuthorityFixture() (*Manager, *fakeStore) {
	m, st, _, _ := newManager()
	st.coordination = domain.CoordinationConfig{CoordinatorProjectID: "coord"}
	st.sessions["target-1"] = mkLive("target-1")
	st.sessions["coord-1"] = domain.SessionRecord{ID: "coord-1", ProjectID: "coord", Kind: domain.KindOrchestrator}
	st.sessions["repo-mgr-1"] = domain.SessionRecord{ID: "repo-mgr-1", ProjectID: "mer", Kind: domain.KindOrchestrator}
	st.sessions["worker-1"] = domain.SessionRecord{ID: "worker-1", ProjectID: "coord", Kind: domain.KindWorker}
	st.sessions["coord-old"] = domain.SessionRecord{ID: "coord-old", ProjectID: "coord", Kind: domain.KindOrchestrator, IsTerminated: true}
	return m, st
}

func TestSetAutoResumePolicyAuthority(t *testing.T) {
	cases := []struct {
		name    string
		sender  domain.SessionID
		policy  domain.AutoResumePolicy
		wantErr error
	}{
		{"human lifts never", "", domain.AutoResumeAllowed, nil},
		{"human sets never", "", domain.AutoResumeNever, nil},
		{"coordinator sets never", "coord-1", domain.AutoResumeNever, nil},
		{"coordinator may not lift never", "coord-1", domain.AutoResumeAllowed, ErrAutoResumeNotPermitted},
		{"repo manager may not set never", "repo-mgr-1", domain.AutoResumeNever, ErrAutoResumeNotPermitted},
		{"repo manager may not lift never", "repo-mgr-1", domain.AutoResumeAllowed, ErrAutoResumeNotPermitted},
		{"worker is refused", "worker-1", domain.AutoResumeNever, ErrAutoResumeNotPermitted},
		{"terminated coordinator is refused", "coord-old", domain.AutoResumeNever, ErrAutoResumeNotPermitted},
		{"unknown sender is refused", "ghost-1", domain.AutoResumeNever, ErrAutoResumeNotPermitted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, st := autoResumeAuthorityFixture()
			st.protected = map[domain.SessionID]bool{"target-1": tc.policy == domain.AutoResumeAllowed}

			err := m.SetAutoResumePolicy(ctx, "target-1", tc.policy, tc.sender)

			if !errors.Is(err, tc.wantErr) || (tc.wantErr == nil && err != nil) {
				t.Fatalf("SetAutoResumePolicy = %v, want %v", err, tc.wantErr)
			}
			if tc.wantErr != nil {
				// A refusal leaves the policy exactly as it was.
				if _, wrote := st.policies["target-1"]; wrote {
					t.Fatalf("a refused change wrote the policy: %v", st.policies)
				}
				return
			}
			if got := st.policies["target-1"]; got != tc.policy {
				t.Fatalf("stored policy = %q, want %q", got, tc.policy)
			}
			if st.protected["target-1"] != !tc.policy.Allows() {
				t.Fatalf("protection did not follow the policy: %v", st.protected)
			}
		})
	}
}

func TestSetAutoResumePolicyRejectsUnknownSessionsAndInvalidValues(t *testing.T) {
	m, st := autoResumeAuthorityFixture()
	if err := m.SetAutoResumePolicy(ctx, "missing-1", domain.AutoResumeNever, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown session = %v, want ErrNotFound", err)
	}
	if err := m.SetAutoResumePolicy(ctx, "target-1", domain.AutoResumePolicy("maybe"), ""); err == nil {
		t.Fatal("an invalid policy value was accepted")
	}
	if len(st.policies) != 0 {
		t.Fatalf("a rejected call wrote a policy: %v", st.policies)
	}
}

// Without a coordinator project configured, no session is "the coordinator", so an
// agent sender can never set the policy.
func TestSetAutoResumePolicyWithoutACoordinatorRefusesEveryAgent(t *testing.T) {
	m, st := autoResumeAuthorityFixture()
	st.coordination = domain.CoordinationConfig{}
	if err := m.SetAutoResumePolicy(ctx, "target-1", domain.AutoResumeNever, "coord-1"); !errors.Is(err, ErrAutoResumeNotPermitted) {
		t.Fatalf("agent sender with no coordinator = %v, want ErrAutoResumeNotPermitted", err)
	}
}
