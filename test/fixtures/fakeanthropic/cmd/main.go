// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

// Command fakeprovider serves the fake Anthropic endpoint in a cluster, for the
// end-to-end injection test.
//
// It is a test fixture. It exists so the e2e can prove the injection webhook
// wired a real sidecar to a real upstream without needing a funded API key or
// egress from the test cluster.
package main

import (
	"flag"
	"log"
	"net/http"

	"github.com/waveoffai/waveoff/test/fixtures/fakeanthropic"
)

func main() {
	addr := flag.String("listen", ":8000", "address to bind")
	flag.Parse()

	api := fakeanthropic.New(
		fakeanthropic.Turn{Text: "Laptops can be returned within 30 days."},
	)
	mux := http.NewServeMux()
	mux.Handle("/", api)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	log.Printf("fake provider listening on %s", *addr)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatal(err)
	}
}
