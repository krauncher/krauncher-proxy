// SPDX-License-Identifier: Apache-2.0

// Command fakeupstream serves a simulated OpenAI-compatible chat API.
package main

import (
	"flag"
	"log"
	"net/http"
	"time"

	"github.com/krauncher/krauncher-proxy/internal/fakeupstream"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8081", "listen address")
	var o fakeupstream.Options
	flag.IntVar(&o.Tokens, "tokens", 128, "completion tokens when the request sets no max_tokens")
	flag.DurationVar(&o.HeaderDelay, "header-delay", 200*time.Millisecond, "delay before response headers")
	flag.DurationVar(&o.TokenInterval, "token-interval", 20*time.Millisecond, "delay between streamed tokens")
	flag.Parse()
	srv := &http.Server{Addr: *addr, Handler: fakeupstream.Handler(o), ReadHeaderTimeout: 10 * time.Second}
	log.Printf("fakeupstream on %s", *addr)
	log.Fatal(srv.ListenAndServe())
}
