// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/krauncher/krauncher-proxy/internal/config"
)

func tokensFile(t *testing.T, content string) string {
	p := filepath.Join(t.TempDir(), "tokens")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestHeaderAuth(t *testing.T) {
	f := tokensFile(t, "# clients\napp-a:"+HashToken("secret-a")+"\n\napp-b:"+HashToken("secret-b")+"\n")
	a, err := New(config.ClientAuth{Mode: config.AuthHeader, Header: "X-Proxy-Key", TokensFile: f})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		token, client, reason string
		ok                    bool
	}{
		{"secret-b", "app-b", "", true},
		{"secret-a", "app-a", "", true},
		{"", "", ReasonMissing, false},
		{"wrong", "", ReasonInvalid, false},
	}
	for _, c := range cases {
		r, _ := http.NewRequest(http.MethodGet, "http://x/", nil)
		if c.token != "" {
			r.Header.Set("X-Proxy-Key", c.token)
		}
		r.Header.Set("Authorization", "Bearer upstream")
		client, reason, ok := a.Check(r)
		if client != c.client || reason != c.reason || ok != c.ok {
			t.Errorf("token %q: %q %q %v", c.token, client, reason, ok)
		}
		if r.Header.Get("X-Proxy-Key") != "" || r.Header.Get("Authorization") != "Bearer upstream" {
			t.Errorf("token %q: headers after check %v", c.token, r.Header)
		}
	}
	if a.Failures(ReasonMissing) != 1 || a.Failures(ReasonInvalid) != 1 {
		t.Errorf("failure counts %d %d", a.Failures(ReasonMissing), a.Failures(ReasonInvalid))
	}
}

func TestTokensFileErrors(t *testing.T) {
	for _, content := range []string{"", "noseparator\n", "a:nothex\n", "a:abcd\n", ":" + HashToken("x") + "\n"} {
		if _, err := LoadTokens(tokensFile(t, content)); err == nil {
			t.Errorf("%q accepted", content)
		}
	}
	if _, err := LoadTokens("/nonexistent"); err == nil {
		t.Error("missing file accepted")
	}
}

func TestMTLSAuth(t *testing.T) {
	cert := &x509.Certificate{Subject: pkix.Name{CommonName: "svc-a"}, DNSNames: []string{"svc-a.internal"}}
	req := func(c *x509.Certificate) *http.Request {
		r, _ := http.NewRequest(http.MethodGet, "http://x/", nil)
		if c != nil {
			r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{c}}
		}
		return r
	}
	a, _ := New(config.ClientAuth{Mode: config.AuthMTLS, MTLS: config.MTLS{NameFrom: "cn", AllowedNames: []string{"svc-a"}}})
	if name, _, ok := a.Check(req(cert)); !ok || name != "svc-a" {
		t.Errorf("cn: %q %v", name, ok)
	}
	if _, reason, ok := a.Check(req(nil)); ok || reason != ReasonMissing {
		t.Errorf("no cert: %q %v", reason, ok)
	}
	other := &x509.Certificate{Subject: pkix.Name{CommonName: "svc-b"}}
	if _, reason, ok := a.Check(req(other)); ok || reason != ReasonCertNotAllowed {
		t.Errorf("not allowed: %q %v", reason, ok)
	}
	san, _ := New(config.ClientAuth{Mode: config.AuthMTLS, MTLS: config.MTLS{NameFrom: "san_dns"}})
	if name, _, ok := san.Check(req(cert)); !ok || name != "svc-a.internal" {
		t.Errorf("san: %q %v", name, ok)
	}
}

func TestOffIsNil(t *testing.T) {
	a, err := New(config.ClientAuth{Mode: config.AuthOff})
	if a != nil || err != nil {
		t.Fatal("off mode built an authenticator")
	}
	if _, _, ok := a.Check(nil); !ok {
		t.Fatal("nil authenticator rejected")
	}
}
