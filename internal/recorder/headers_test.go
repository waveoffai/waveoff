// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package recorder_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/waveoffai/waveoff/internal/cassette"
	"github.com/waveoffai/waveoff/internal/recorder"
)

func TestHeadersAreRecordedAndRedacted(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("anthropic-version", "2023-06-01")
		w.Header().Set("Set-Cookie", "session=secret")
		io.WriteString(w, "{}")
	}))
	defer up.Close()

	c := &collector{}
	px := newProxy(t, up.URL, c)
	req, _ := http.NewRequest("POST", px.URL, http.NoBody)
	req.Header.Set("x-api-key", "sk-ant-api03-REALKEY1234567890abcdefgh")
	req.Header.Set("content-type", "application/json")
	resp, _ := http.DefaultClient.Do(req)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	r := c.all()[0]
	if r.ReqHeader.Get("x-api-key") == "sk-ant-api03-REALKEY1234567890abcdefgh" {
		t.Error("the API key survived into the record")
	}
	if r.ReqHeader.Get("content-type") != "application/json" {
		t.Error("a harmless header was destroyed")
	}
	if len(r.ReqRedacted) == 0 {
		t.Error("nothing was reported as redacted")
	}

	attrs := recorder.SpanAttributes(r)
	respHdrs, ok := attrs[cassette.AttrResponseHeaders].(map[string]string)
	if !ok {
		t.Fatalf("response headers missing from the span: %v", attrs)
	}
	// §5 pins the provider's resolved model version, which arrives as a header.
	if respHdrs["anthropic-version"] != "2023-06-01" {
		t.Errorf("the provider version header was not recorded: %v", respHdrs)
	}
	if respHdrs["set-cookie"] == "session=secret" {
		t.Error("a Set-Cookie survived into the span")
	}
}
