// SPDX-License-Identifier: Apache-2.0

// Command llm-shape-proxy is a transparent LLM API proxy that records the
// workload shape of the traffic. See doc/.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"

	"github.com/krauncher/krauncher-proxy/internal/auth"
	"github.com/krauncher/krauncher-proxy/internal/config"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = ""

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("llm-shape-proxy", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "path to the YAML configuration file")
	check := fs.Bool("check", false, "validate the configuration and exit")
	showVersion := fs.Bool("version", false, "print the version and exit")
	hashToken := fs.Bool("hash-token", false, "read a token from stdin, print its SHA-256 for the tokens file")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	switch {
	case *showVersion:
		fmt.Fprintln(stdout, buildVersion())
		return 0
	case *hashToken:
		line, err := bufio.NewReader(stdin).ReadString('\n')
		if err != nil && err != io.EOF {
			fmt.Fprintln(stderr, err)
			return 1
		}
		token := strings.TrimRight(line, "\r\n")
		if token == "" {
			fmt.Fprintln(stderr, "empty token")
			return 1
		}
		fmt.Fprintln(stdout, auth.HashToken(token))
		return 0
	}

	if *configPath == "" {
		fmt.Fprintln(stderr, "-config is required")
		return 2
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if *check {
		fmt.Fprintln(stdout, "configuration OK")
		return 0
	}

	log := newLogger(cfg.Log, stderr)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := serve(ctx, cfg, log); err != nil {
		log.Error("fatal", "err", err)
		return 1
	}
	return 0
}

func newLogger(c config.Log, w io.Writer) *slog.Logger {
	var level slog.Level
	_ = level.UnmarshalText([]byte(c.Level)) // validated by config
	opts := &slog.HandlerOptions{Level: level}
	if c.Format == "text" {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}

func buildVersion() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
		return bi.Main.Version
	}
	return "dev"
}
