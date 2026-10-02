// SPDX-License-Identifier: Apache-2.0

// Package config defines the proxy configuration, its defaults, loading from
// YAML with environment overrides, and validation. See doc/configuration.md.
package config

import "time"

// Config is the complete proxy configuration.
type Config struct {
	Instance   Instance   `yaml:"instance"`
	Listen     Listen     `yaml:"listen"`
	ClientAuth ClientAuth `yaml:"client_auth"`
	Routes     []Route    `yaml:"routes"`
	Upstream   Upstream   `yaml:"upstream"`
	Limits     Limits     `yaml:"limits"`
	Capture    Capture    `yaml:"capture"`
	Pipeline   Pipeline   `yaml:"pipeline"`
	Estimate   Estimate   `yaml:"estimate"`
	Prefix     Prefix     `yaml:"prefix"`
	Metrics    Metrics    `yaml:"metrics"`
	Sink       Sink       `yaml:"sink"`
	Security   Security   `yaml:"security"`
	Log        Log        `yaml:"log"`
	Shutdown   Shutdown   `yaml:"shutdown"`
}

type Instance struct {
	Name string `yaml:"name"`
}

// TLS holds certificate paths. An empty CertFile means plain HTTP.
type TLS struct {
	CertFile     string `yaml:"cert_file"`
	KeyFile      string `yaml:"key_file"`
	ClientCAFile string `yaml:"client_ca_file"`
}

// Enabled reports whether a certificate is configured.
func (t TLS) Enabled() bool { return t.CertFile != "" }

type Listen struct {
	Addr              string        `yaml:"addr"`
	TLS               TLS           `yaml:"tls"`
	ReadHeaderTimeout time.Duration `yaml:"read_header_timeout"`
	IdleTimeout       time.Duration `yaml:"idle_timeout"`
}

// Client authentication modes.
const (
	AuthOff    = "off"
	AuthHeader = "header"
	AuthMTLS   = "mtls"
)

type ClientAuth struct {
	Mode       string `yaml:"mode"`
	Header     string `yaml:"header"`
	TokensFile string `yaml:"tokens_file"`
	MTLS       MTLS   `yaml:"mtls"`
}

type MTLS struct {
	NameFrom     string   `yaml:"name_from"`
	AllowedNames []string `yaml:"allowed_names"`
}

// Dialects and stream usage modes of a route.
const (
	DialectOpenAI    = "openai"
	DialectAnthropic = "anthropic"
	DialectGeneric   = "generic"

	StreamUsagePassthrough = "passthrough"
	StreamUsageInject      = "inject"
	StreamUsageOff         = "off"
)

type Route struct {
	Name        string `yaml:"name"`
	Prefix      string `yaml:"prefix"`
	Upstream    string `yaml:"upstream"`
	StripPrefix bool   `yaml:"strip_prefix"`
	Dialect     string `yaml:"dialect"`
	Precision   string `yaml:"precision"`
	Engine      string `yaml:"engine"`
	StreamUsage string `yaml:"stream_usage"`
}

type Upstream struct {
	MaxIdleConns          int           `yaml:"max_idle_conns"`
	MaxIdleConnsPerHost   int           `yaml:"max_idle_conns_per_host"`
	MaxConnsPerHost       int           `yaml:"max_conns_per_host"`
	IdleConnTimeout       time.Duration `yaml:"idle_conn_timeout"`
	DialTimeout           time.Duration `yaml:"dial_timeout"`
	TLSHandshakeTimeout   time.Duration `yaml:"tls_handshake_timeout"`
	ResponseHeaderTimeout time.Duration `yaml:"response_header_timeout"`
	ForceHTTP2            bool          `yaml:"force_http2"`
}

type Limits struct {
	MaxInflight int `yaml:"max_inflight"`
}

type Capture struct {
	Enabled           bool `yaml:"enabled"`
	RequestMaxBytes   Size `yaml:"request_max_bytes"`
	ResponseMaxBytes  Size `yaml:"response_max_bytes"`
	ResponseHeadBytes Size `yaml:"response_head_bytes"`
	ResponseTailBytes Size `yaml:"response_tail_bytes"`
	TimelineMaxPoints int  `yaml:"timeline_max_points"`
	BudgetBytes       Size `yaml:"budget_bytes"`
	BudgetStep        Size `yaml:"budget_step"`
}

type Pipeline struct {
	QueueSize   int           `yaml:"queue_size"`
	Workers     int           `yaml:"workers"`
	RequestWait time.Duration `yaml:"request_wait"`
}

type Estimate struct {
	BytesPerToken    float64 `yaml:"bytes_per_token"`
	TokensPerMessage int     `yaml:"tokens_per_message"`
}

type Prefix struct {
	Enabled    bool          `yaml:"enabled"`
	BlockBytes Size          `yaml:"block_bytes"`
	MaxEntries int           `yaml:"max_entries"`
	TTL        time.Duration `yaml:"ttl"`
}

type Metrics struct {
	Listen           string    `yaml:"listen"`
	TLS              TLS       `yaml:"tls"`
	BearerTokenFile  string    `yaml:"bearer_token_file"`
	Pprof            bool      `yaml:"pprof"`
	Models           []string  `yaml:"models"`
	MaxModels        int       `yaml:"max_models"`
	NativeHistograms bool      `yaml:"native_histograms"`
	ClientLabel      bool      `yaml:"client_label"`
	ShapeCell        ShapeCell `yaml:"shape_cell"`
}

// ShapeCell holds the bucket upper bounds; +inf is implied after the last.
type ShapeCell struct {
	PromptBounds []int `yaml:"prompt_bounds"`
	OutputBounds []int `yaml:"output_bounds"`
}

type Sink struct {
	JSONL JSONL `yaml:"jsonl"`
}

type JSONL struct {
	Enabled        bool          `yaml:"enabled"`
	Dir            string        `yaml:"dir"`
	Retention      time.Duration `yaml:"retention"`
	MaxTotalBytes  Size          `yaml:"max_total_bytes"`
	TimeResolution time.Duration `yaml:"time_resolution"`
	MaxBytes       Size          `yaml:"max_bytes"`
	MaxAge         time.Duration `yaml:"max_age"`
	Gzip           bool          `yaml:"gzip"`
	Batch          int           `yaml:"batch"`
	FlushInterval  time.Duration `yaml:"flush_interval"`
	QueueSize      int           `yaml:"queue_size"`
}

type Security struct {
	Strict bool `yaml:"strict"`
}

type Log struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

type Shutdown struct {
	Grace time.Duration `yaml:"grace"`
}

// Default returns the configuration with every default from
// doc/configuration.md applied. Routes have no default.
func Default() Config {
	return Config{
		Listen: Listen{
			Addr:              ":8080",
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       120 * time.Second,
		},
		ClientAuth: ClientAuth{
			Mode:   AuthOff,
			Header: "X-Proxy-Key",
			MTLS:   MTLS{NameFrom: "cn"},
		},
		Upstream: Upstream{
			MaxIdleConns:        4096,
			MaxIdleConnsPerHost: 1024,
			IdleConnTimeout:     90 * time.Second,
			DialTimeout:         5 * time.Second,
			TLSHandshakeTimeout: 5 * time.Second,
			ForceHTTP2:          true,
		},
		Capture: Capture{
			Enabled:           true,
			RequestMaxBytes:   1 * MiB,
			ResponseMaxBytes:  1 * MiB,
			ResponseHeadBytes: 16 * KiB,
			ResponseTailBytes: 64 * KiB,
			TimelineMaxPoints: 256,
			BudgetBytes:       2 * GiB,
			BudgetStep:        64 * KiB,
		},
		Pipeline: Pipeline{
			QueueSize:   65536,
			RequestWait: 100 * time.Millisecond,
		},
		Estimate: Estimate{BytesPerToken: 4.0, TokensPerMessage: 4},
		Prefix: Prefix{
			BlockBytes: 1 * KiB,
			MaxEntries: 1000000,
			TTL:        time.Hour,
		},
		Metrics: Metrics{
			Listen:    "127.0.0.1:9090",
			MaxModels: 50,
			ShapeCell: ShapeCell{
				PromptBounds: []int{512, 1024, 2048, 4096, 8192, 16384, 32768},
				OutputBounds: []int{32, 64, 128, 256, 512, 1024, 2048},
			},
		},
		Sink: Sink{JSONL: JSONL{
			Dir:            "./data",
			Retention:      168 * time.Hour,
			MaxTotalBytes:  10 * GiB,
			TimeResolution: time.Millisecond,
			MaxBytes:       256 * MiB,
			MaxAge:         time.Hour,
			Gzip:           true,
			Batch:          1024,
			FlushInterval:  time.Second,
			QueueSize:      65536,
		}},
		Log:      Log{Level: "info", Format: "json"},
		Shutdown: Shutdown{Grace: 60 * time.Second},
	}
}
