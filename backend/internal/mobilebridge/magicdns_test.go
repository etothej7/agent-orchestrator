package mobilebridge

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

const statusJSON = `{
  "Self": {"DNSName": "prasads-macbook-pro.tail057d04.ts.net."},
  "CertDomains": ["prasads-macbook-pro.tail057d04.ts.net"],
  "MagicDNSSuffix": "tail057d04.ts.net"
}`

func fixedRunner(out string, err error) TailscaleRunner {
	return func(ctx context.Context, args ...string) ([]byte, error) { return []byte(out), err }
}

func TestQueryTailscaleParsesNameAndCerts(t *testing.T) {
	got := queryTailscale(context.Background(), fixedRunner(statusJSON, nil))
	if got.Name != "prasads-macbook-pro.tail057d04.ts.net" {
		t.Errorf("Name = %q, want the name with no trailing dot", got.Name)
	}
	if !got.CertsEnabled {
		t.Error("CertsEnabled = false, want true when CertDomains is non-empty")
	}
}

func TestQueryTailscaleCertsDisabled(t *testing.T) {
	const noCerts = `{"Self":{"DNSName":"host.tail1.ts.net."},"CertDomains":[]}`
	got := queryTailscale(context.Background(), fixedRunner(noCerts, nil))
	if got.Name != "host.tail1.ts.net" {
		t.Errorf("Name = %q", got.Name)
	}
	if got.CertsEnabled {
		t.Error("CertsEnabled = true, want false for an empty CertDomains")
	}
}

// Every failure mode must yield the zero value rather than a partial result:
// the caller treats an empty Name as "secure pairing unavailable".
func TestQueryTailscaleFailuresYieldZero(t *testing.T) {
	cases := map[string]struct {
		out string
		err error
	}{
		"binary missing": {"", errors.New("exec: \"tailscale\": executable file not found in $PATH")},
		"non-zero exit":  {"", errors.New("exit status 1")},
		"malformed json": {"not json", nil},
		"empty DNSName":  {`{"Self":{"DNSName":""},"CertDomains":["x"]}`, nil},
		"missing Self":   {`{"CertDomains":["x"]}`, nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := queryTailscale(context.Background(), fixedRunner(tc.out, tc.err)); got.Name != "" {
				t.Errorf("Name = %q, want empty", got.Name)
			}
		})
	}
}

// The name goes into a QR and an endpoint as secure:true, so it must look like a
// MagicDNS name and be one the node can actually get a certificate for — not
// merely coexist with some other cert domain.
func TestQueryTailscaleValidatesNameShapeAndCertMembership(t *testing.T) {
	mk := func(name string, certs ...string) string {
		b, _ := json.Marshal(map[string]any{"Self": map[string]string{"DNSName": name}, "CertDomains": certs})
		return string(b)
	}
	cases := []struct {
		name      string
		json      string
		wantName  string
		wantCerts bool
	}{
		{"valid and in cert domains", mk("box.tail1.ts.net.", "box.tail1.ts.net"), "box.tail1.ts.net", true},
		{"case-insensitive match", mk("Box.Tail1.ts.net.", "box.tail1.ts.net"), "box.tail1.ts.net", true},
		{"cert domains list another host", mk("box.tail1.ts.net.", "other.tail1.ts.net"), "box.tail1.ts.net", false},
		{"not a ts.net name", mk("box.example.com.", "box.example.com"), "", false},
		{"port in name", mk("box.tail1.ts.net:8443.", "box.tail1.ts.net:8443"), "", false},
		{"slash in name", mk("box.tail1.ts.net/x.", "box.tail1.ts.net/x"), "", false},
		{"userinfo in name", mk("a@box.tail1.ts.net.", "a@box.tail1.ts.net"), "", false},
		{"space in name", mk("bo x.tail1.ts.net.", "bo x.tail1.ts.net"), "", false},
		{"bare ts.net", mk("ts.net.", "ts.net"), "", false},
		{"empty label", mk(".tail1.ts.net.", ".tail1.ts.net"), "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := queryTailscale(context.Background(), fixedRunner(tc.json, nil))
			if got.Name != tc.wantName || got.CertsEnabled != tc.wantCerts {
				t.Errorf("got name-set=%v certs=%v, want name-set=%v certs=%v", got.Name != "", got.CertsEnabled, tc.wantName != "", tc.wantCerts)
			}
			if tc.wantName != "" && got.Name != tc.wantName {
				t.Errorf("name = %q, want %q", got.Name, tc.wantName)
			}
		})
	}
}
