package uicapability

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
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


type vectorFile struct {
	Secret  string `json:"secret"`
	NowMs   int64  `json:"nowMs"`
	Vectors []struct {
		Name   string `json:"name"`
		Method string `json:"method"`
		URL    string `json:"url"`
		Path   string `json:"path"`
		Header string `json:"header"`
	} `json:"vectors"`
}

// The same file is read by frontend/src/main/ui-capability.test.ts. A path that the Electron
// signer (new URL(url).pathname) and the daemon (net/http EscapedPath) spell differently
// would make every such request fail verification, so each vector pins the spelling on both
// sides and the signature that results.
func TestSharedVectorsWithTheElectronSignerIncludingHardPaths(t *testing.T) {
	raw, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var file vectorFile
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	if len(file.Vectors) == 0 {
		t.Fatal("no vectors")
	}
	now := time.UnixMilli(file.NowMs)
	verifier := New(file.Secret)
	for _, v := range file.Vectors {
		t.Run(v.Name, func(t *testing.T) {
			if got := Sign(file.Secret, PurposeHumanWrite, RequestSubject(v.Method, v.Path), now); got != v.Header {
				t.Fatalf("Sign = %q, vector %q", got, v.Header)
			}
			req := httptest.NewRequest(v.Method, v.URL, nil)
			if got := req.URL.EscapedPath(); got != v.Path {
				t.Fatalf("EscapedPath = %q, the Electron signer signs %q", got, v.Path)
			}
			if err := verifier.VerifyRequest(v.Header, req.Method, req.URL.EscapedPath(), now); err != nil {
				t.Fatalf("daemon refuses the signer's header: %v", err)
			}
			if err := verifier.VerifyRequest(v.Header, req.Method, req.URL.EscapedPath()+"x", now); err == nil {
				t.Fatal("a different path was accepted")
			}
		})
	}
}

func TestValidKey(t *testing.T) {
	good := strings.Repeat("a", 43)
	for _, tc := range []struct {
		key  string
		want bool
	}{{good, true}, {strings.Repeat("A-_9", 11), true}, {"", false}, {"short", false}, {good + "=", false}, {good + " ", false}, {strings.Repeat("é", 40), false}} {
		if got := ValidKey(tc.key); got != tc.want {
			t.Errorf("ValidKey(%q) = %v, want %v", tc.key, got, tc.want)
		}
	}
}
