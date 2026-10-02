// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

const minimal = `
routes:
  - name: main
    prefix: /
    upstream: https://api.example.com
    dialect: openai
`

func noEnv(string) (string, bool) { return "", false }

func envMap(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestExampleConfigIsValidAndMatchesDefaults(t *testing.T) {
	data, err := os.ReadFile("../../configs/example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Parse(data, noEnv)
	if err != nil {
		t.Fatalf("example config invalid: %v", err)
	}
	def, err := Parse([]byte(minimal), noEnv)
	if err != nil {
		t.Fatal(err)
	}
	// The example documents the defaults: apart from routes and the derived
	// instance name, it must decode to the same values.
	cfg.Routes, def.Routes = nil, nil
	// Compared as printed values: an empty list in YAML and a nil default are equal.
	if fmt.Sprintf("%+v", cfg) != fmt.Sprintf("%+v", def) {
		t.Errorf("example differs from defaults:\nexample:  %+v\ndefaults: %+v", cfg, def)
	}
}

func TestParseSize(t *testing.T) {
	cases := map[string]Size{"512B": 512, "64KiB": 64 * KiB, "1MiB": MiB, "2GiB": 2 * GiB, "100": 100, " 3 MiB ": 3 * MiB}
	for in, want := range cases {
		got, err := ParseSize(in)
		if err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "1MB", "-1", "1.5MiB", "KiB"} {
		if _, err := ParseSize(in); err == nil {
			t.Errorf("ParseSize(%q) succeeded, want error", in)
		}
	}
}

func TestEnvOverrides(t *testing.T) {
	cfg, err := Parse([]byte(minimal), envMap(map[string]string{
		"LLM_SHAPE_CAPTURE_BUDGET_BYTES":             "512MiB",
		"LLM_SHAPE_SINK_JSONL_ENABLED":               "true",
		"LLM_SHAPE_PIPELINE_REQUEST_WAIT":            "250ms",
		"LLM_SHAPE_METRICS_MODELS":                   "[a, b]",
		"LLM_SHAPE_METRICS_SHAPE_CELL_OUTPUT_BOUNDS": "[10, 20]",
		"LLM_SHAPE_CLIENT_AUTH_MTLS_NAME_FROM":       "san_dns",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Capture.BudgetBytes != 512*MiB || !cfg.Sink.JSONL.Enabled ||
		cfg.Pipeline.RequestWait != 250*time.Millisecond ||
		len(cfg.Metrics.Models) != 2 || cfg.Metrics.ShapeCell.OutputBounds[1] != 20 ||
		cfg.ClientAuth.MTLS.NameFrom != "san_dns" {
		t.Errorf("overrides not applied: %+v", cfg)
	}
}

func TestEnvOverrideBadValueNamesKey(t *testing.T) {
	_, err := Parse([]byte(minimal), envMap(map[string]string{"LLM_SHAPE_CAPTURE_BUDGET_BYTES": "lots"}))
	if err == nil || !strings.Contains(err.Error(), "LLM_SHAPE_CAPTURE_BUDGET_BYTES") {
		t.Fatalf("want error naming the variable, got %v", err)
	}
}

func TestRouteDefaults(t *testing.T) {
	cfg, err := Parse([]byte(minimal), noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Routes[0].StreamUsage != StreamUsagePassthrough {
		t.Errorf("stream_usage default = %q", cfg.Routes[0].StreamUsage)
	}
	if cfg.Instance.Name == "" {
		t.Error("instance name not derived from hostname")
	}
}

// Each case is a YAML document and a key the error must name.
func TestValidationErrorsNameTheKey(t *testing.T) {
	route := func(extra string) string {
		return "routes:\n  - name: main\n    prefix: /\n    upstream: https://api.example.com\n    dialect: openai\n" + extra
	}
	cases := []struct{ name, yaml, key string }{
		{"unknown key", minimal + "listen:\n  adr: x\n", "adr"},
		{"no routes", "listen:\n  addr: \":8080\"\n", "routes"},
		{"bad dialect", strings.Replace(minimal, "openai", "gpt", 1), "routes[0].dialect"},
		{"bad upstream", strings.Replace(minimal, "https://api.example.com", "api.example.com", 1), "routes[0].upstream"},
		{"bad prefix", strings.Replace(minimal, "prefix: /", "prefix: v1", 1), "routes[0].prefix"},
		{"duplicate prefix", minimal + "  - name: two\n    prefix: /\n    upstream: https://b.example.com\n    dialect: openai\n", "routes[1].prefix"},
		{"bad precision", route("    precision: BF16\n"), "routes[0].precision"},
		{"bad stream_usage", route("    stream_usage: always\n"), "routes[0].stream_usage"},
		{"same listeners", minimal + "metrics:\n  listen: \":8080\"\n", "metrics.listen"},
		{"tls half", minimal + "listen:\n  tls:\n    cert_file: c.pem\n", "listen.tls"},
		{"header without tokens", minimal + "client_auth:\n  mode: header\n", "client_auth.tokens_file"},
		{"mtls without ca", minimal + "client_auth:\n  mode: mtls\n", "listen.tls.client_ca_file"},
		{"bad auth mode", minimal + "client_auth:\n  mode: basic\n", "client_auth.mode"},
		{"bounds not increasing", minimal + "metrics:\n  shape_cell:\n    prompt_bounds: [10, 5]\n", "metrics.shape_cell.prompt_bounds"},
		{"budget below cap", minimal + "capture:\n  budget_bytes: 1KiB\n", "capture.budget_bytes"},
		{"bad log level", minimal + "log:\n  level: loud\n", "log.level"},
		{"jsonl no dir", minimal + "sink:\n  jsonl:\n    enabled: true\n    dir: \"\"\n", "sink.jsonl.dir"},
		{"bad size", minimal + "capture:\n  request_max_bytes: 1MB\n", "1MB"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.yaml), noEnv)
			if err == nil || !strings.Contains(err.Error(), c.key) {
				t.Fatalf("want error naming %q, got %v", c.key, err)
			}
		})
	}
}

func TestStrictMode(t *testing.T) {
	base := minimal + "security:\n  strict: true\n"
	_, err := Parse([]byte(base), noEnv)
	if err == nil {
		t.Fatal("strict mode accepted a plain listener on all interfaces")
	}
	for _, want := range []string{"requires listen.tls", "requires client_auth"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("strict error missing %q: %v", want, err)
		}
	}

	ok := minimal + `
security:
  strict: true
listen:
  addr: "0.0.0.0:8443"
  tls: {cert_file: c.pem, key_file: k.pem, client_ca_file: ca.pem}
client_auth:
  mode: mtls
`
	if _, err := Parse([]byte(ok), noEnv); err != nil {
		t.Fatalf("valid strict config rejected: %v", err)
	}

	inject := strings.Replace(ok, "dialect: openai", "dialect: openai\n    stream_usage: inject", 1)
	if _, err := Parse([]byte(inject), noEnv); err == nil || !strings.Contains(err.Error(), "stream_usage inject") {
		t.Fatalf("strict mode accepted inject: %v", err)
	}
}

func TestIsLoopback(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:9090": true, "[::1]:9090": true, "localhost:9090": true,
		":9090": false, "0.0.0.0:9090": false, "10.0.0.1:9090": false, "bad": false,
	}
	for addr, want := range cases {
		if got := IsLoopback(addr); got != want {
			t.Errorf("IsLoopback(%q) = %v, want %v", addr, got, want)
		}
	}
}
