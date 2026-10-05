package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSessionID(t *testing.T) {
	mk := func(body string, hdr map[string]string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		return r
	}
	if got := sessionIDFromRequest(mk(`{}`, map[string]string{"session_id": "abc"})); got != "abc" {
		t.Fatalf("header session = %q", got)
	}
	r := mk(`{"prompt_cache_key":"conv-1"}`, nil)
	a := sessionIDFromRequest(r)
	if a == processSessionID || a != sessionIDFromRequest(mk(`{"prompt_cache_key":"conv-1"}`, nil)) {
		t.Fatalf("body session not stable: %q", a)
	}
	if b, _ := io.ReadAll(r.Body); !strings.Contains(string(b), "conv-1") {
		t.Fatal("body not restored")
	}
	if got := sessionIDFromRequest(mk(`{}`, nil)); got != processSessionID {
		t.Fatalf("fallback = %q", got)
	}
}
