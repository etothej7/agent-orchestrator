package httpd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/requestscope"
	"github.com/aoagents/agent-orchestrator/backend/internal/uicapability"
)

func humanCapabilityProbe(v *uicapability.Verifier, now time.Time, method, path, header string) (human bool) {
	h := humanCapabilityMiddleware(v, func() time.Time { return now })(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		human = requestscope.IsHumanUI(r.Context())
	}))
	req := httptest.NewRequest(method, path, nil)
	if header != "" {
		req.Header.Set(uicapability.Header, header)
	}
	h.ServeHTTP(httptest.NewRecorder(), req)
	return human
}

func TestHumanCapabilityMiddlewareMarksOnlyAValidRequestBoundCapability(t *testing.T) {
	const secret = "run-secret"
	now := time.Unix(1793102400, 0)
	v := uicapability.New(secret)
	sign := func(method, path string, at time.Time) string {
		return uicapability.Sign(secret, uicapability.PurposeHumanWrite, uicapability.RequestSubject(method, path), at)
	}
	path := "/api/v1/sessions/s-1/conversation/messages"

	if !humanCapabilityProbe(v, now, http.MethodPost, path, sign(http.MethodPost, path, now)) {
		t.Fatal("a fresh capability for this exact request must mark it human")
	}
	cases := map[string]struct {
		v      *uicapability.Verifier
		method string
		header string
	}{
		"missing":             {v, http.MethodPost, ""},
		"other path":          {v, http.MethodPost, sign(http.MethodPost, "/api/v1/sessions/s-2/conversation/messages", now)},
		"other method":        {v, http.MethodPost, sign(http.MethodPut, path, now)},
		"stale":               {v, http.MethodPost, sign(http.MethodPost, path, now.Add(-2*uicapability.Window))},
		"previous app run":    {v, http.MethodPost, uicapability.Sign("previous-run-secret", uicapability.PurposeHumanWrite, uicapability.RequestSubject(http.MethodPost, path), now)},
		"another purpose":     {v, http.MethodPost, uicapability.Sign(secret, "other", uicapability.RequestSubject(http.MethodPost, path), now)},
		"garbage":             {v, http.MethodPost, "v1.1.x"},
		"no verifier (agent)": {nil, http.MethodPost, sign(http.MethodPost, path, now)},
	}
	for name, tc := range cases {
		if humanCapabilityProbe(tc.v, now, tc.method, path, tc.header) {
			t.Errorf("%s: request was marked human", name)
		}
	}
}

func probe(t *testing.T, v *uicapability.Verifier, body string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:3001/internal/human-capability/probe", strings.NewReader(body))
	if mutate != nil {
		mutate(req)
	}
	rec := httptest.NewRecorder()
	humanCapabilityProbeHandler(v).ServeHTTP(rec, req)
	return rec
}

func TestTheProbeProvesPossessionOfTheKeyToALocalNativeCallerOnly(t *testing.T) {
	key := strings.Repeat("k", 43)
	nonce := "AbCdEfGhIjKlMnOpQrStUvWxYz012345"
	rec := probe(t, uicapability.New(key), `{"nonce":"`+nonce+`"}`, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), uicapability.ProbeProof(key, nonce)) {
		t.Fatalf("probe = %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), key) {
		t.Fatal("the probe answer contains the key")
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("the proof may be cached")
	}
	// A daemon with another key answers differently, so the app can tell it is not the one it started.
	other := probe(t, uicapability.New(strings.Repeat("z", 43)), `{"nonce":"`+nonce+`"}`, nil)
	if strings.Contains(other.Body.String(), uicapability.ProbeProof(key, nonce)) {
		t.Fatal("a daemon with another key produced this run's proof")
	}
}

func TestTheProbeRefusesWhatCouldMakeItAnOracleOrLeakWhoHoldsAKey(t *testing.T) {
	good := `{"nonce":"AbCdEfGhIjKlMnOpQrStUvWxYz012345"}`
	keyed := uicapability.New(strings.Repeat("k", 43))
	for name, tc := range map[string]struct {
		v      *uicapability.Verifier
		body   string
		mutate func(*http.Request)
		want   int
	}{
		"no key (a daemon the app did not start)": {nil, good, nil, http.StatusNotFound},
		"empty key":                 {uicapability.New(""), good, nil, http.StatusNotFound},
		"short nonce":               {keyed, `{"nonce":"short"}`, nil, http.StatusNotFound},
		"nonce with odd characters": {keyed, `{"nonce":"AbCdEfGhIjKlMnOpQrStUv WxYz012345"}`, nil, http.StatusNotFound},
		"not json":                  {keyed, `nonce`, nil, http.StatusBadRequest},
		"a browser (any Origin)":    {keyed, good, func(r *http.Request) { r.Header.Set("Origin", "app://renderer") }, http.StatusForbidden},
		"a rebinding host":          {keyed, good, func(r *http.Request) { r.Host = "evil.example:3001" }, http.StatusForbidden},
	} {
		if rec := probe(t, tc.v, tc.body, tc.mutate); rec.Code != tc.want {
			t.Errorf("%s: status %d, want %d (%s)", name, rec.Code, tc.want, rec.Body.String())
		}
	}
}
