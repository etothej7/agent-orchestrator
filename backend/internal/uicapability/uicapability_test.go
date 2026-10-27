package uicapability

import (
	"errors"
	"testing"
	"time"
)

var now = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

func TestARequestCapabilityIsBoundToItsMethodAndPath(t *testing.T) {
	v := New("run-secret")
	subject := RequestSubject("post", "/api/v1/sessions/optimus_db-1/auto-resume/lift")
	c := Sign("run-secret", PurposeHumanWrite, subject, now)
	if err := v.VerifyRequest(c, "POST", "/api/v1/sessions/optimus_db-1/auto-resume/lift", now); err != nil {
		t.Fatalf("a capability for exactly this request was refused: %v", err)
	}
	for name, tc := range map[string][2]string{
		"another method":  {"PUT", "/api/v1/sessions/optimus_db-1/auto-resume/lift"},
		"another session": {"POST", "/api/v1/sessions/optimus_rails_app-1/auto-resume/lift"},
		"another path":    {"POST", "/api/v1/sessions/optimus_db-1/conversation/messages"},
	} {
		if err := v.VerifyRequest(c, tc[0], tc[1], now); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: a replayed capability = %v, want ErrInvalid", name, err)
		}
	}
}

func TestAValidCapabilityIsAcceptedOnlyForItsPurposeAndSubject(t *testing.T) {
	v := New("run-secret")
	c := Sign("run-secret", PurposeHumanWrite, "optimus_db-1", now)
	if err := v.Verify(c, PurposeHumanWrite, "optimus_db-1", now); err != nil {
		t.Fatalf("valid capability refused: %v", err)
	}
	if err := v.Verify(c, PurposeHumanWrite, "optimus_rails_app-1", now); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a capability for one session worked for another: %v", err)
	}
	if err := v.Verify(c, "something-else", "optimus_db-1", now); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a capability for one purpose worked for another: %v", err)
	}
}

func TestStaleFutureWrongRunAndMalformedCapabilitiesAreRefused(t *testing.T) {
	v := New("run-secret")
	cases := map[string]struct {
		header string
		want   error
	}{
		"missing":                {"", ErrMissing},
		"whitespace":             {"   ", ErrMissing},
		"stale":                  {Sign("run-secret", PurposeHumanWrite, "s", now.Add(-2*Window)), ErrExpired},
		"from the future":        {Sign("run-secret", PurposeHumanWrite, "s", now.Add(2*Window)), ErrExpired},
		"another app run":        {Sign("other-run-secret", PurposeHumanWrite, "s", now), ErrInvalid},
		"empty secret signature": {Sign("", PurposeHumanWrite, "s", now), ErrInvalid},
		"garbage":                {"not-a-capability", ErrInvalid},
		"wrong version":          {"v2.1.abc", ErrInvalid},
		"bad timestamp":          {"v1.notanumber.abc", ErrInvalid},
		"truncated mac":          {Sign("run-secret", PurposeHumanWrite, "s", now)[:30], ErrInvalid},
		"bearer of the secret":   {"run-secret", ErrInvalid},
	}
	for name, tc := range cases {
		if err := v.Verify(tc.header, PurposeHumanWrite, "s", now); !errors.Is(err, tc.want) {
			t.Errorf("%s: error = %v, want %v", name, err, tc.want)
		}
	}
}

// A timestamp edited after signing breaks the MAC; a window edge still passes.
func TestATamperedTimestampBreaksTheMACAndTheWindowEdgeStillPasses(t *testing.T) {
	v := New("run-secret")
	signed := Sign("run-secret", PurposeHumanWrite, "s", now.Add(-Window))
	if err := v.Verify(signed, PurposeHumanWrite, "s", now); err != nil {
		t.Fatalf("a capability exactly at the window edge was refused: %v", err)
	}
	tampered := "v1.9999999999." + signed[len("v1.1234567890."):]
	if err := v.Verify(tampered, PurposeHumanWrite, "s", now); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a tampered timestamp = %v, want ErrInvalid", err)
	}
}

// A daemon that was not handed a secret (headless) can lift nothing.
func TestAVerifierWithoutASecretRefusesEverything(t *testing.T) {
	for _, v := range []*Verifier{nil, {}, New("")} {
		if err := v.Verify(Sign("", PurposeHumanWrite, "s", now), PurposeHumanWrite, "s", now); err == nil {
			t.Fatal("a verifier with no secret accepted a capability")
		}
		if err := v.Verify(Sign("x", PurposeHumanWrite, "s", now), PurposeHumanWrite, "s", now); err == nil {
			t.Fatal("a verifier with no secret accepted a capability")
		}
	}
}

// Fixed vector shared with the Electron signer's test: both sides must agree byte for byte.
func TestSharedVectorWithTheElectronSigner(t *testing.T) {
	const want = "v1.1793102400.8VJhn2Mul_MHx3kNbKsRwyjuZlHU3YmE7pkfdvRUMos"
	got := Sign("test-secret", PurposeHumanWrite, RequestSubject("POST", "/api/v1/sessions/sess-1/auto-resume/lift"), time.Unix(1793102400, 0))
	if got != want {
		t.Fatalf("Sign = %q, want %q (update frontend/src/main/ui-capability.test.ts too)", got, want)
	}
}

func TestDeriveSecretSharedVector(t *testing.T) {
	const want = "TFmczbvMaO3L6eBh9WJalDFprS--z2nHeaXiUAiKO7U"
	if got := DeriveSecret("test-token"); got != want {
		t.Fatalf("DeriveSecret = %q, want %q (update frontend/src/main/ui-capability.test.ts too)", got, want)
	}
	if DeriveSecret("") != "" {
		t.Fatal("an empty runtime token must derive no secret")
	}
	if DeriveSecret("a") == DeriveSecret("b") {
		t.Fatal("different tokens must derive different secrets")
	}
}
