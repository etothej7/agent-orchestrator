package httpd

import (
	"net/http"
	"net/http/httptest"
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
