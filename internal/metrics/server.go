// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/krauncher/krauncher-proxy/internal/config"
)

// Server is the metrics listener: /metrics, /healthz, /readyz and, when
// enabled, /debug/pprof. It never shares the proxy listener.
type Server struct {
	srv *http.Server
	ln  net.Listener
	tls bool
}

// NewServer prepares the listener; Serve starts it.
func NewServer(c config.Metrics, m *Metrics) (*Server, error) {
	var token []byte
	if c.BearerTokenFile != "" {
		b, err := os.ReadFile(c.BearerTokenFile)
		if err != nil {
			return nil, fmt.Errorf("metrics.bearer_token_file: %w", err)
		}
		t := strings.TrimSpace(string(b))
		if t == "" {
			return nil, fmt.Errorf("metrics.bearer_token_file: empty")
		}
		sum := sha256.Sum256([]byte(t))
		token = sum[:]
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(m.Registry(), promhttp.HandlerOpts{}))
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	mux.Handle("/healthz", ok)
	mux.Handle("/readyz", ok)
	if c.Pprof {
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	}
	var h http.Handler = mux
	if token != nil {
		h = bearer(token, mux)
	}
	s := &Server{srv: &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}}
	if c.TLS.Enabled() {
		cfg := &tls.Config{MinVersion: tls.VersionTLS12}
		cert, err := tls.LoadX509KeyPair(c.TLS.CertFile, c.TLS.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("metrics.tls: %w", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
		if c.TLS.ClientCAFile != "" {
			pool, err := LoadCAPool(c.TLS.ClientCAFile)
			if err != nil {
				return nil, fmt.Errorf("metrics.tls.client_ca_file: %w", err)
			}
			cfg.ClientCAs, cfg.ClientAuth = pool, tls.RequireAndVerifyClientCert
		}
		s.srv.TLSConfig, s.tls = cfg, true
	}
	ln, err := net.Listen("tcp", c.Listen)
	if err != nil {
		return nil, fmt.Errorf("metrics.listen: %w", err)
	}
	s.ln = ln
	return s, nil
}

// Addr returns the bound address.
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Serve blocks until Close.
func (s *Server) Serve() error {
	var err error
	if s.tls {
		err = s.srv.ServeTLS(s.ln, "", "")
	} else {
		err = s.srv.Serve(s.ln)
	}
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// Close stops the listener immediately; scrapes are short.
func (s *Server) Close() error { return s.srv.Close() }

// bearer requires "Authorization: Bearer <token>", compared in constant time.
func bearer(tokenHash []byte, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		sum := sha256.Sum256([]byte(got))
		if !ok || subtle.ConstantTimeCompare(sum[:], tokenHash) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// LoadCAPool reads PEM certificates.
func LoadCAPool(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("%s: no certificates", path)
	}
	return pool, nil
}
