package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

var (
	chatGPTResponsesURL = "https://chatgpt.com/backend-api/codex/responses"
	openAIResponsesURL  = "https://api.openai.com/v1/responses"
)

var (
	bundledSlugsOnce sync.Once
	bundledSlugs     map[string]bool
)

// isCodexBundledModel reports whether model is one of the normal Codex/OpenAI
// models that ships with the codex binary.
func isCodexBundledModel(model string) bool {
	bundledSlugsOnce.Do(func() {
		bundledSlugs = map[string]bool{}
		for _, raw := range loadBundledCodexModels() {
			var head struct {
				Slug string `json:"slug"`
			}
			if json.Unmarshal(raw, &head) == nil && head.Slug != "" {
				bundledSlugs[head.Slug] = true
			}
		}
	})
	return bundledSlugs[model]
}

// shouldPassthroughToOpenAI is true for normal Codex models that OpenCode Go
// does not serve and that the user has not remapped.
func shouldPassthroughToOpenAI(model string) bool {
	if isLocalModel(model) {
		return false
	}
	if !isCodexBundledModel(model) {
		return false
	}
	if mappings, err := loadModelMappings(); err == nil {
		if _, ok := mappings["codex"][model]; ok {
			return false
		}
	}
	for _, id := range knownModelIDs() {
		if id == model {
			return false
		}
	}
	return true
}

// proxyOpenAIPassthrough forwards a Responses request unchanged to OpenAI,
// using the credentials Codex itself attached. A ChatGPT login (identified by
// the ChatGPT-Account-ID header) goes to the ChatGPT Codex backend, an API key
// to api.openai.com.
func proxyOpenAIPassthrough(w http.ResponseWriter, r *http.Request, body []byte) {
	proxyOpenAIPassthroughTo(w, r, body, chatGPTResponsesURL, openAIResponsesURL)
}

func proxyOpenAIPassthroughTo(w http.ResponseWriter, r *http.Request, body []byte, chatgptURL, apiURL string) {
	upstream := apiURL
	if r.Header.Get("ChatGPT-Account-ID") != "" {
		upstream = chatgptURL
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upstream, bytes.NewReader(body))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	for k, vs := range r.Header {
		switch http.CanonicalHeaderKey(k) {
		case "Host", "Content-Length", "Accept-Encoding", "Connection":
			continue
		}
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	client := &http.Client{} // no overall timeout: responses stream
	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		if http.CanonicalHeaderKey(k) == "Content-Length" {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	streamBody(w, resp.Body)
}

func streamBody(w http.ResponseWriter, body io.Reader) {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 4096)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

var openCodeResponsesURL = "https://opencode.ai/zen/go/v1/responses"

// proxyOpenCodeResponses forwards a Codex Responses request to OpenCode Go's
// native /v1/responses endpoint, for models that only support that protocol.
func proxyOpenCodeResponses(w http.ResponseWriter, r *http.Request, cfg Config, body []byte, model string) {
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if isLocalModel(model) {
		http.Error(w, "local models do not use the OpenCode Go endpoint", http.StatusInternalServerError)
		return
	}
	req["model"] = model
	// Codex-specific fields OpenCode Go models don't support; use model defaults.
	for _, key := range []string{"reasoning", "service_tier", "include"} {
		delete(req, key)
	}
	out, err := json.Marshal(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	up, err := http.NewRequestWithContext(r.Context(), http.MethodPost, openCodeResponsesURL, bytes.NewReader(out))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	up.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	up.Header.Set("Content-Type", "application/json")
	if accept := r.Header.Get("Accept"); accept != "" {
		up.Header.Set("Accept", accept)
	}
	setUpstreamHeaders(up)
	resp, err := (&http.Client{}).Do(up)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	streamBody(w, resp.Body)
}

func codexAuthFile() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex", "auth.json")
}

// codexHasChatGPTLogin reports whether Codex is logged in with ChatGPT.
func codexHasChatGPTLogin() bool {
	b, err := os.ReadFile(codexAuthFile())
	if err != nil {
		return false
	}
	var auth struct {
		AuthMode string `json:"auth_mode"`
		Tokens   *struct {
			AccessToken string `json:"access_token"`
		} `json:"tokens"`
	}
	if json.Unmarshal(b, &auth) != nil {
		return false
	}
	return strings.EqualFold(auth.AuthMode, "chatgpt") || (auth.Tokens != nil && auth.Tokens.AccessToken != "")
}

// proxyResponsesCompact serves Codex's remote-compaction endpoint. Only OpenAI
// models can compact remotely; OpenCode Go has no such endpoint, so Codex is
// told to use its local compaction instead.
func proxyResponsesCompact(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	var head struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &head)
	if !shouldPassthroughToOpenAI(head.Model) {
		http.Error(w, "remote compaction is not supported for OpenCode Go models", http.StatusNotImplemented)
		return
	}
	proxyOpenAIPassthroughTo(w, r, body, chatGPTResponsesURL+"/compact", openAIResponsesURL+"/compact")
}
