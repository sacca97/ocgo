package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenAIPassthrough(t *testing.T) {
	var gotAuth, gotBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotAuth, gotBody = r.Header.Get("Authorization"), string(b)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: ok\n\n"))
	}))
	defer up.Close()
	oldC, oldA, oldB := chatGPTResponsesURL, openAIResponsesURL, loadBundledCodexModels
	chatGPTResponsesURL, openAIResponsesURL = up.URL, up.URL
	loadBundledCodexModels = func() []json.RawMessage { return []json.RawMessage{json.RawMessage(`{"slug":"gpt-5.5"}`)} }
	bundledSlugsOnce.Do(func() {}) // reset below
	defer func() { chatGPTResponsesURL, openAIResponsesURL, loadBundledCodexModels = oldC, oldA, oldB }()
	bundledSlugs = map[string]bool{"gpt-5.5": true}

	if !shouldPassthroughToOpenAI("gpt-5.5") {
		t.Fatal("gpt-5.5 should pass through")
	}
	if shouldPassthroughToOpenAI("kimi-k2.6") {
		t.Fatal("kimi-k2.6 must not pass through")
	}
	body := `{"model":"gpt-5.5","input":[]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer chatgpt-token")
	req.Header.Set("ChatGPT-Account-ID", "acct")
	rec := httptest.NewRecorder()
	proxyResponses(rec, req, Config{APIKey: "opencode-key"})
	if gotAuth != "Bearer chatgpt-token" || gotBody != body || !strings.Contains(rec.Body.String(), "data: ok") {
		t.Fatalf("auth=%q body=%q resp=%q", gotAuth, gotBody, rec.Body.String())
	}
}
