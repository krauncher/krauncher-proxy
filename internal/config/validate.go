// SPDX-License-Identifier: Apache-2.0

package config

import (
	"cmp"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// labelRe limits operator-declared route labels (precision, engine).
var labelRe = regexp.MustCompile(`^[a-z0-9._-]{0,32}$`)

// problems collects validation errors, each naming the offending key.
type problems []error

func (p *problems) addf(key, format string, args ...any) {
	*p = append(*p, fmt.Errorf("%s: %s", key, fmt.Sprintf(format, args...)))
}

func oneOf[T comparable](p *problems, key string, v T, allowed ...T) {
	if !slices.Contains(allowed, v) {
		p.addf(key, "%v is not one of %v", v, allowed)
	}
}

func positive[T cmp.Ordered](p *problems, key string, v T) {
	var zero T
	if v <= zero {
		p.addf(key, "must be > 0, got %v", v)
	}
}

func nonNegative[T cmp.Ordered](p *problems, key string, v T) {
	var zero T
	if v < zero {
		p.addf(key, "must be >= 0, got %v", v)
	}
}

func increasing(p *problems, key string, v []int) {
	for i, b := range v {
		if b <= 0 || (i > 0 && b <= v[i-1]) {
			p.addf(key, "bounds must be positive and strictly increasing, got %v", v)
			return
		}
	}
}

func tlsPair(p *problems, key string, t TLS) {
	if (t.CertFile == "") != (t.KeyFile == "") {
		p.addf(key, "cert_file and key_file must be set together")
	}
	if t.ClientCAFile != "" && t.CertFile == "" {
		p.addf(key+".client_ca_file", "requires cert_file and key_file")
	}
}

// sameListener reports whether two listen addresses would bind the same
// port: equal ports, and equal hosts or either host binding all interfaces.
func sameListener(a, b string) bool {
	ha, pa, errA := net.SplitHostPort(a)
	hb, pb, errB := net.SplitHostPort(b)
	if errA != nil || errB != nil {
		return a == b
	}
	if pa != pb {
		return false
	}
	wildcard := func(h string) bool { return h == "" || h == "0.0.0.0" || h == "::" }
	return wildcard(ha) || wildcard(hb) || strings.EqualFold(ha, hb)
}

// IsLoopback reports whether a listen address binds only to loopback.
// An empty host (":8080") binds all interfaces and is not loopback.
func IsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Validate checks the configuration against the rules in
// doc/configuration.md and returns all problems found, joined.
func (c *Config) Validate() error {
	var p problems

	if _, _, err := net.SplitHostPort(c.Listen.Addr); err != nil {
		p.addf("listen.addr", "%v", err)
	}
	if _, _, err := net.SplitHostPort(c.Metrics.Listen); err != nil {
		p.addf("metrics.listen", "%v", err)
	}
	if sameListener(c.Listen.Addr, c.Metrics.Listen) {
		p.addf("metrics.listen", "must not use the same address and port as listen.addr")
	}
	tlsPair(&p, "listen.tls", c.Listen.TLS)
	tlsPair(&p, "metrics.tls", c.Metrics.TLS)
	nonNegative(&p, "listen.read_header_timeout", c.Listen.ReadHeaderTimeout)
	nonNegative(&p, "listen.idle_timeout", c.Listen.IdleTimeout)

	c.validateClientAuth(&p)
	c.validateRoutes(&p)

	nonNegative(&p, "upstream.max_idle_conns", c.Upstream.MaxIdleConns)
	nonNegative(&p, "upstream.max_idle_conns_per_host", c.Upstream.MaxIdleConnsPerHost)
	nonNegative(&p, "upstream.max_conns_per_host", c.Upstream.MaxConnsPerHost)
	nonNegative(&p, "upstream.response_header_timeout", c.Upstream.ResponseHeaderTimeout)
	positive(&p, "upstream.dial_timeout", c.Upstream.DialTimeout)
	positive(&p, "upstream.tls_handshake_timeout", c.Upstream.TLSHandshakeTimeout)
	nonNegative(&p, "limits.max_inflight", c.Limits.MaxInflight)

	if c.Capture.Enabled {
		positive(&p, "capture.request_max_bytes", c.Capture.RequestMaxBytes)
		positive(&p, "capture.response_max_bytes", c.Capture.ResponseMaxBytes)
		positive(&p, "capture.response_head_bytes", c.Capture.ResponseHeadBytes)
		positive(&p, "capture.response_tail_bytes", c.Capture.ResponseTailBytes)
		positive(&p, "capture.timeline_max_points", c.Capture.TimelineMaxPoints)
		positive(&p, "capture.budget_step", c.Capture.BudgetStep)
		if c.Capture.BudgetBytes < c.Capture.RequestMaxBytes {
			p.addf("capture.budget_bytes", "must be >= capture.request_max_bytes")
		}
	}

	positive(&p, "pipeline.queue_size", c.Pipeline.QueueSize)
	nonNegative(&p, "pipeline.workers", c.Pipeline.Workers)
	positive(&p, "pipeline.request_wait", c.Pipeline.RequestWait)
	positive(&p, "estimate.bytes_per_token", c.Estimate.BytesPerToken)
	nonNegative(&p, "estimate.tokens_per_message", c.Estimate.TokensPerMessage)

	if c.Prefix.Enabled {
		positive(&p, "prefix.block_bytes", c.Prefix.BlockBytes)
		positive(&p, "prefix.max_entries", c.Prefix.MaxEntries)
		positive(&p, "prefix.ttl", c.Prefix.TTL)
	}

	positive(&p, "metrics.max_models", c.Metrics.MaxModels)
	increasing(&p, "metrics.shape_cell.prompt_bounds", c.Metrics.ShapeCell.PromptBounds)
	increasing(&p, "metrics.shape_cell.output_bounds", c.Metrics.ShapeCell.OutputBounds)

	if j := c.Sink.JSONL; j.Enabled {
		if j.Dir == "" {
			p.addf("sink.jsonl.dir", "required when sink.jsonl.enabled")
		}
		positive(&p, "sink.jsonl.retention", j.Retention)
		positive(&p, "sink.jsonl.max_total_bytes", j.MaxTotalBytes)
		positive(&p, "sink.jsonl.time_resolution", j.TimeResolution)
		positive(&p, "sink.jsonl.max_bytes", j.MaxBytes)
		positive(&p, "sink.jsonl.max_age", j.MaxAge)
		positive(&p, "sink.jsonl.batch", j.Batch)
		positive(&p, "sink.jsonl.flush_interval", j.FlushInterval)
		positive(&p, "sink.jsonl.queue_size", j.QueueSize)
	}

	oneOf(&p, "log.level", c.Log.Level, "debug", "info", "warn", "error")
	oneOf(&p, "log.format", c.Log.Format, "json", "text")
	nonNegative(&p, "shutdown.grace", c.Shutdown.Grace)

	if c.Security.Strict {
		c.validateStrict(&p)
	}
	c.rejectUnimplemented(&p)
	return errors.Join(p...)
}

// rejectUnimplemented refuses options whose behaviour does not exist yet, so a
// configuration never promises something the proxy does not do. Remove a line
// when its feature lands.
func (c *Config) rejectUnimplemented(p *problems) {
	if c.Prefix.Enabled {
		p.addf("prefix.enabled", "not implemented yet")
	}
}

func (c *Config) validateClientAuth(p *problems) {
	a := c.ClientAuth
	oneOf(p, "client_auth.mode", a.Mode, AuthOff, AuthHeader, AuthMTLS)
	switch a.Mode {
	case AuthHeader:
		if a.Header == "" {
			p.addf("client_auth.header", "required for mode header")
		}
		if a.TokensFile == "" {
			p.addf("client_auth.tokens_file", "required for mode header")
		}
	case AuthMTLS:
		if c.Listen.TLS.ClientCAFile == "" {
			p.addf("listen.tls.client_ca_file", "required for client_auth.mode mtls")
		}
		oneOf(p, "client_auth.mtls.name_from", a.MTLS.NameFrom, "cn", "san_dns")
	}
	if c.Listen.TLS.ClientCAFile != "" && a.Mode != AuthMTLS {
		p.addf("listen.tls.client_ca_file", "only used with client_auth.mode mtls")
	}
}

func (c *Config) validateRoutes(p *problems) {
	if len(c.Routes) == 0 {
		p.addf("routes", "at least one route is required")
	}
	names, prefixes := map[string]bool{}, map[string]bool{}
	for i, r := range c.Routes {
		key := fmt.Sprintf("routes[%d]", i)
		if r.Name == "" {
			p.addf(key+".name", "required")
		} else if names[r.Name] {
			p.addf(key+".name", "duplicate %q", r.Name)
		}
		names[r.Name] = true
		if !strings.HasPrefix(r.Prefix, "/") {
			p.addf(key+".prefix", "must start with /, got %q", r.Prefix)
		} else if prefixes[r.Prefix] {
			p.addf(key+".prefix", "duplicate %q", r.Prefix)
		}
		prefixes[r.Prefix] = true
		if u, err := url.Parse(r.Upstream); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			p.addf(key+".upstream", "must be an absolute http or https URL, got %q", r.Upstream)
		}
		oneOf(p, key+".dialect", r.Dialect, DialectOpenAI, DialectAnthropic, DialectGeneric)
		oneOf(p, key+".stream_usage", r.StreamUsage, StreamUsagePassthrough, StreamUsageInject, StreamUsageOff)
		if !labelRe.MatchString(r.Precision) {
			p.addf(key+".precision", "must match %s", labelRe)
		}
		if !labelRe.MatchString(r.Engine) {
			p.addf(key+".engine", "must match %s", labelRe)
		}
	}
}

func (c *Config) validateStrict(p *problems) {
	const key = "security.strict"
	if !c.Listen.TLS.Enabled() {
		p.addf(key, "requires listen.tls")
	}
	if c.Metrics.Pprof {
		p.addf(key, "forbids metrics.pprof")
	}
	for i, r := range c.Routes {
		if r.StreamUsage == StreamUsageInject {
			p.addf(key, "forbids routes[%d].stream_usage inject", i)
		}
	}
	if !IsLoopback(c.Metrics.Listen) && !c.Metrics.TLS.Enabled() {
		p.addf(key, "requires metrics.listen on loopback or metrics.tls")
	}
	if !IsLoopback(c.Listen.Addr) && c.ClientAuth.Mode == AuthOff {
		p.addf(key, "requires client_auth when listen.addr is not loopback")
	}
}
