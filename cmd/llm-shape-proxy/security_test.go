// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
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
	"github.com/krauncher/krauncher-proxy/internal/testpki"
)

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
	pki := testpki.New(t)
	certFile, keyFile, _ := pki.Issue(t, "proxy", true)
	tokens := filepath.Join(pki.Dir, "tokens")
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
	c := pki.Client()
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
	pki := testpki.New(t)
	certFile, keyFile, _ := pki.Issue(t, "proxy", true)
	_, _, allowed := pki.Issue(t, "svc-a", false)
	_, _, other := pki.Issue(t, "svc-b", false)

	cfg := serveConfig(t, up.URL, time.Second)
	cfg.Listen.TLS = config.TLS{CertFile: certFile, KeyFile: keyFile, ClientCAFile: pki.CAFile}
	cfg.ClientAuth = config.ClientAuth{Mode: config.AuthMTLS, MTLS: config.MTLS{NameFrom: "cn", AllowedNames: []string{"svc-a"}}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cancel, done := startServe(t, cfg)
	defer func() { cancel(); <-done }()
	url := "https://" + cfg.Listen.Addr + "/x"

	if code, _, err := get(pki.Client(allowed), url, nil); err != nil || code != 200 {
		t.Errorf("allowed certificate: %d %v", code, err)
	}
	if code, _, err := get(pki.Client(other), url, nil); err != nil || code != 401 {
		t.Errorf("certificate outside the allowlist: %d %v", code, err)
	}
	if _, _, err := get(pki.Client(), url, nil); err == nil {
		t.Error("connection without a client certificate accepted")
	}
}

// A certificate name that breaks the export rules reaches the record as
// "invalid", never as written.
func TestServeMTLSClientNameSanitized(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	defer up.Close()
	pki := testpki.New(t)
	certFile, keyFile, _ := pki.Issue(t, "proxy", true)
	_, _, odd := pki.Issue(t, "name with spaces; DROP", false)

	cfg := serveConfig(t, up.URL, time.Second)
	cfg.Listen.TLS = config.TLS{CertFile: certFile, KeyFile: keyFile, ClientCAFile: pki.CAFile}
	cfg.ClientAuth = config.ClientAuth{Mode: config.AuthMTLS, MTLS: config.MTLS{NameFrom: "cn"}} // any CA-signed name
	cancel, done := startServe(t, cfg)
	if code, _, err := get(pki.Client(odd), "https://"+cfg.Listen.Addr+"/x", nil); err != nil || code != 200 {
		t.Fatalf("request: %d %v", code, err)
	}
	cancel()
	<-done
	lines := jsonlLines(t, cfg)
	if len(lines) != 1 || !strings.Contains(lines[0], `"client":"invalid"`) || strings.Contains(lines[0], "DROP") {
		t.Fatalf("records %v", lines)
	}
}
