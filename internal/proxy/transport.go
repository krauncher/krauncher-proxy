// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"crypto/tls"
	"net"
	"net/http"
	"time"

	"github.com/krauncher/krauncher-proxy/internal/config"
)

// NewTransport builds the shared upstream transport. TLS verification is
// always on: there is deliberately no option to disable it.
func NewTransport(c config.Upstream) *http.Transport {
	dialer := &net.Dialer{Timeout: c.DialTimeout, KeepAlive: 30 * time.Second}
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     c.ForceHTTP2,
		MaxIdleConns:          c.MaxIdleConns,
		MaxIdleConnsPerHost:   c.MaxIdleConnsPerHost,
		MaxConnsPerHost:       c.MaxConnsPerHost,
		IdleConnTimeout:       c.IdleConnTimeout,
		TLSHandshakeTimeout:   c.TLSHandshakeTimeout,
		ResponseHeaderTimeout: c.ResponseHeaderTimeout,
		// The client's Accept-Encoding passes through untouched; the transport
		// must neither add gzip nor decompress, or the bytes would change.
		DisableCompression: true,
		TLSClientConfig:    &tls.Config{MinVersion: tls.VersionTLS12},
	}
}
