// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/krauncher/krauncher-proxy/internal/auth"
	"github.com/krauncher/krauncher-proxy/internal/config"
)

// testPKI is a CA with helpers to issue server and client certificates.
type testPKI struct {
	dir    string
	ca     *x509.Certificate
	caKey  *ecdsa.PrivateKey
	caFile string
	pool   *x509.CertPool
}

func newPKI(t *testing.T) *testPKI {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	ca, _ := x509.ParseCertificate(der)
	p := &testPKI{dir: t.TempDir(), ca: ca, caKey: key, pool: x509.NewCertPool()}
	p.pool.AddCert(ca)
	p.caFile = p.write("ca.pem", "CERTIFICATE", der)
	return p
}

func (p *testPKI) write(name, typ string, der []byte) string {
	path := filepath.Join(p.dir, name)
	os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600)
	return path
}

// issue returns a certificate and key signed by the CA, as files and as a
// tls.Certificate.
func (p *testPKI) issue(t *testing.T, name string, server bool) (certFile, keyFile string, c tls.Certificate) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature,
	}
	if server {
		tmpl.ExtKeyUsage, tmpl.IPAddresses = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, []net.IP{net.ParseIP("127.0.0.1")}
	} else {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.ca, &key.PublicKey, p.caKey)
	if err != nil {
		t.Fatal(err)
	}
	kder, _ := x509.MarshalECPrivateKey(key)
	certFile, keyFile = p.write(name+".pem", "CERTIFICATE", der), p.write(name+".key", "EC PRIVATE KEY", kder)
	c, err = tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile, c
}

func (p *testPKI) client(certs ...tls.Certificate) *http.Client {
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: p.pool, Certificates: certs}}}
}

func waitListening(t *testing.T, addr string) {
	for range 100 {
		if c, err := net.Dial("tcp", addr); err == nil {
			c.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("not listening")
}

func get(c *http.Client, url string, header map[string]string) (int, string, error) {
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), nil
}

func TestServeTLSWithHeaderAuthAndMetrics(t *testing.T) {
	seen := make(chan http.Header, 10)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Clone()
		io.WriteString(w, "ok")
	}))
	defer up.Close()
	pki := newPKI(t)
	certFile, keyFile, _ := pki.issue(t, "proxy", true)
	tokens := filepath.Join(pki.dir, "tokens")
	os.WriteFile(tokens, []byte("app-a:"+auth.HashToken("token-a")+"\n"), 0o600)

	cfg := serveConfig(t, up.URL, time.Second)
	cfg.Listen.TLS = config.TLS{CertFile: certFile, KeyFile: keyFile}
	cfg.ClientAuth = config.ClientAuth{Mode: config.AuthHeader, Header: "X-Proxy-Key", TokensFile: tokens}
	cfg.Metrics.ClientLabel = true
	cfg.Security.Strict = true // TLS + client auth + loopback metrics satisfy it
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cancel, done := startServe(t, cfg)
	defer func() { cancel(); <-done }()
	c := pki.client()
	url := "https://" + cfg.Listen.Addr + "/v1/models"

	if code, _, err := get(c, url, nil); err != nil || code != 401 {
		t.Fatalf("no token: %d %v", code, err)
	}
	if code, _, _ := get(c, url, map[string]string{"X-Proxy-Key": "wrong"}); code != 401 {
		t.Fatalf("wrong token: %d", code)
	}
	code, body, err := get(c, url, map[string]string{"X-Proxy-Key": "token-a", "Authorization": "Bearer upstream-key"})
	if err != nil || code != 200 || body != "ok" {
		t.Fatalf("valid token: %d %q %v", code, body, err)
	}
	h := <-seen
	if h.Get("X-Proxy-Key") != "" || h.Get("Authorization") != "Bearer upstream-key" {
		t.Errorf("upstream headers %v", h)
	}
	select {
	case <-seen:
		t.Error("an unauthenticated request reached the upstream")
	default:
	}
	// net/http answers plain HTTP on a TLS port with 400 and closes.
	if code, _, err := get(&http.Client{}, "http://"+cfg.Listen.Addr+"/v1/models", nil); err == nil && code != 400 {
		t.Errorf("plain HTTP on the TLS listener: %d", code)
	}
	select {
	case <-seen:
		t.Error("a plain-HTTP request reached the upstream")
	default:
	}

	var metrics string
	for range 50 { // records reach the metrics through stage 2
		_, metrics, _ = get(&http.Client{}, "http://"+cfg.Metrics.Listen+"/metrics", nil)
		if strings.Contains(metrics, `client="app-a"`) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, want := range []string{
		`llm_shape_auth_failures_total{reason="missing"} 1`,
		`llm_shape_auth_failures_total{reason="invalid"} 1`,
		`outcome="unauthorized"`,
		`client="app-a"`,
		`llm_shape_inflight{route="main"} 0`,
	} {
		if !strings.Contains(metrics, want) {
			t.Errorf("metrics lack %s", want)
		}
	}
}

func TestServeMTLS(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	defer up.Close()
	pki := newPKI(t)
	certFile, keyFile, _ := pki.issue(t, "proxy", true)
	_, _, allowed := pki.issue(t, "svc-a", false)
	_, _, other := pki.issue(t, "svc-b", false)

	cfg := serveConfig(t, up.URL, time.Second)
	cfg.Listen.TLS = config.TLS{CertFile: certFile, KeyFile: keyFile, ClientCAFile: pki.caFile}
	cfg.ClientAuth = config.ClientAuth{Mode: config.AuthMTLS, MTLS: config.MTLS{NameFrom: "cn", AllowedNames: []string{"svc-a"}}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cancel, done := startServe(t, cfg)
	defer func() { cancel(); <-done }()
	url := "https://" + cfg.Listen.Addr + "/x"

	if code, _, err := get(pki.client(allowed), url, nil); err != nil || code != 200 {
		t.Errorf("allowed certificate: %d %v", code, err)
	}
	if code, _, err := get(pki.client(other), url, nil); err != nil || code != 401 {
		t.Errorf("certificate outside the allowlist: %d %v", code, err)
	}
	if _, _, err := get(pki.client(), url, nil); err == nil {
		t.Error("connection without a client certificate accepted")
	}
}
