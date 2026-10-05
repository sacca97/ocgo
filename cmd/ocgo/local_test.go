package main

import (
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type recorded struct {
	mu    sync.Mutex
	paths []string
	auth  []string
	model []string
}

func (r *recorded) handler(w http.ResponseWriter, req *http.Request) {
	b, _ := io.ReadAll(req.Body)
	var body struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(b, &body)
	r.mu.Lock()
	r.paths = append(r.paths, req.URL.Path)
	r.auth = append(r.auth, req.Header.Get("Authorization"))
	r.model = append(r.model, body.Model)
	r.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"id":"c1","model":"x","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
}

func TestLocalModelRouting(t *testing.T) {
	local, remote := &recorded{}, &recorded{}
	localSrv := httptest.NewServer(http.HandlerFunc(local.handler))
	defer localSrv.Close()
	remoteSrv := httptest.NewServer(http.HandlerFunc(remote.handler))
	defer remoteSrv.Close()
	old := openAIURL
	openAIURL = remoteSrv.URL
	defer func() { openAIURL = old }()

	// No OpenCode key on purpose: a local-only setup must work, and the
	// OpenCode key must never reach the local server.
	cfg := Config{Local: &LocalConfig{URL: localSrv.URL + "/v1", APIKey: "local-key"}}
	cfgWithKey := cfg
	cfgWithKey.APIKey = "opencode-secret"

	post := func(h func(http.ResponseWriter, *http.Request, Config), path, body string, c Config) int {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)), c)
		return rec.Code
	}
	for name, tc := range map[string]struct {
		h    func(http.ResponseWriter, *http.Request, Config)
		path string
		body string
	}{
		"messages":  {proxyMessages, "/v1/messages", `{"model":"local/qwen","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`},
		"responses": {proxyResponses, "/v1/responses", `{"model":"local/qwen","input":"hi"}`},
		"chat":      {proxyChatCompletions, "/v1/chat/completions", `{"model":"local/qwen","messages":[{"role":"user","content":"hi"}]}`},
	} {
		before := len(local.paths)
		if code := post(tc.h, tc.path, tc.body, cfgWithKey); code != 200 {
			t.Fatalf("%s: status %d", name, code)
		}
		if len(local.paths) != before+1 {
			t.Fatalf("%s: request did not reach the local server", name)
		}
		i := len(local.paths) - 1
		if local.paths[i] != "/v1/chat/completions" || local.model[i] != "qwen" || local.auth[i] != "Bearer local-key" {
			t.Fatalf("%s: path=%q model=%q auth=%q", name, local.paths[i], local.model[i], local.auth[i])
		}
	}
	if len(remote.paths) != 0 {
		t.Fatalf("local requests leaked to OpenCode: %v", remote.paths)
	}

	// OpenCode models never reach the local server and keep the OpenCode key.
	if code := post(proxyChatCompletions, "/v1/chat/completions", `{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`, cfgWithKey); code != 200 {
		t.Fatalf("opencode status %d", code)
	}
	if len(remote.paths) != 1 || remote.auth[0] != "Bearer opencode-secret" {
		t.Fatalf("opencode request not routed correctly: %v %v", remote.paths, remote.auth)
	}
	if len(local.paths) != 3 {
		t.Fatalf("opencode request reached the local server: %v", local.paths)
	}
}

func TestLocalModelWithoutConfig(t *testing.T) {
	rec := httptest.NewRecorder()
	proxyChatCompletions(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"local/x","messages":[]}`)), Config{APIKey: "k"})
	if rec.Code < 400 || !strings.Contains(rec.Body.String(), "ocgo local set") {
		t.Fatalf("code=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestFetchLocalInfo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"qwen3-coder"}]}`))
		case "/props":
			_, _ = w.Write([]byte(`{"default_generation_settings":{"n_ctx":16384}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	info, err := fetchLocalInfo(LocalConfig{URL: srv.URL + "/v1"})
	if err != nil || len(info.Models) != 1 || info.Models[0] != "qwen3-coder" || info.ContextWindow != 16384 {
		t.Fatalf("info=%+v err=%v", info, err)
	}
	if _, err := fetchLocalInfo(LocalConfig{URL: "http://127.0.0.1:1"}); err == nil {
		t.Fatal("expected error for unreachable server")
	}
}

func TestLocalModelMetadata(t *testing.T) {
	old := loadLocalConfig
	loadLocalConfig = func() *LocalConfig { return &LocalConfig{ContextWindow: 12345} }
	defer func() { loadLocalConfig = old }()
	meta := modelMetadata("local/x")
	if meta.ContextWindow != 12345 || meta.UsesAnthropicEndpoint || meta.UsesResponsesEndpoint || len(meta.SupportedReasoning) != 0 {
		t.Fatalf("meta=%+v", meta)
	}
	if shouldPassthroughToOpenAI("local/x") || modelUsesAnthropicEndpoint("local/x") {
		t.Fatal("local models must use the chat protocol")
	}
}

func TestLocalSelfSignedTLS(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer local-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"m"}]}`))
	}))
	defer srv.Close()
	pemFile := filepath.Join(t.TempDir(), "ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(pemFile, pemBytes, 0644); err != nil {
		t.Fatal(err)
	}
	base := LocalConfig{URL: srv.URL, APIKey: "local-key"}
	if _, err := fetchLocalInfo(base); err == nil {
		t.Fatal("self-signed certificate should be rejected by default")
	}
	withCA := base
	withCA.CACert = pemFile
	if info, err := fetchLocalInfo(withCA); err != nil || len(info.Models) != 1 {
		t.Fatalf("ca-cert: info=%+v err=%v", info, err)
	}
	insecure := base
	insecure.Insecure = true
	if info, err := fetchLocalInfo(insecure); err != nil || len(info.Models) != 1 {
		t.Fatalf("insecure: info=%+v err=%v", info, err)
	}
	bad := insecure
	bad.APIKey = "wrong"
	if _, err := fetchLocalInfo(bad); err == nil {
		t.Fatal("wrong API key should fail")
	}
}

func TestMappingAcceptsLocalModel(t *testing.T) {
	if knownOpenCodeModel("local/x") {
		t.Fatal("local model must be rejected when no local server is configured")
	}
	old := loadLocalConfig
	loadLocalConfig = func() *LocalConfig { return &LocalConfig{} }
	defer func() { loadLocalConfig = old }()
	if !knownOpenCodeModel("local/x") {
		t.Fatal("local model should be accepted once a local server is configured")
	}
}

func TestClaudeLocalContextEnv(t *testing.T) {
	old := loadLocalConfig
	loadLocalConfig = func() *LocalConfig { return &LocalConfig{ContextWindow: 100000} }
	defer func() { loadLocalConfig = old }()
	m := map[string]map[string]string{"claude": {"claude-haiku": "local/x", "claude-sonnet": "kimi-k3"}}
	if env := claudeLocalContextEnv("", m); len(env) != 0 {
		t.Fatalf("haiku-only local must not limit the session: %v", env)
	}
	m["claude"]["claude-sonnet"] = "local/y"
	if env := claudeLocalContextEnv("", m); len(env) != 1 || env[0] != "CLAUDE_CODE_MAX_CONTEXT_TOKENS=100000" {
		t.Fatalf("env=%v", env)
	}
	if env := claudeLocalContextEnv("local/z", nil); len(env) != 1 {
		t.Fatalf("--model local: env=%v", env)
	}
	if env := claudeLocalContextEnv("kimi-k3", nil); len(env) != 0 {
		t.Fatalf("env=%v", env)
	}
}

func TestLocalErrorIsLoggedAndSaved(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"template boom"}}`, http.StatusInternalServerError)
	}))
	defer srv.Close()
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := Config{Local: &LocalConfig{URL: srv.URL}}
	rec := httptest.NewRecorder()
	proxyChatCompletions(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"local/x","messages":[{"role":"user","content":"hi"}]}`)), cfg)
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), "template boom") {
		t.Fatalf("error body not passed through: %d %q", rec.Code, rec.Body.String())
	}
	b, err := os.ReadFile(filepath.Join(home, ".config", "ocgo", "last-local-error-request.json"))
	if err != nil || !strings.Contains(string(b), `"model":"x"`) {
		t.Fatalf("failing request not saved: %v %q", err, b)
	}
}

func TestClaudeMappingDisplayNameKey(t *testing.T) {
	m := map[string]map[string]string{"claude": {"Haiku 4.5": "local/x"}}
	for _, src := range []string{"claude-haiku-4-5", "claude-haiku-4-5-20251001"} {
		if got := resolveMappedModel("claude", src, m); got != "local/x" {
			t.Fatalf("%s -> %s", src, got)
		}
	}
	if got := resolveMappedModel("claude", "claude-sonnet-5-5", m); got != "claude-sonnet-5-5" {
		t.Fatalf("unrelated model remapped: %s", got)
	}
}

func TestMergeSystemMessages(t *testing.T) {
	var req map[string]any
	_ = json.Unmarshal([]byte(`{"messages":[{"role":"system","content":"a"},{"role":"user","content":"hi"},{"role":"developer","content":[{"type":"text","text":"b"}]},{"role":"assistant","content":"yo"},{"role":"system","content":"c"}]}`), &req)
	mergeSystemMessages(req)
	msgs := req["messages"].([]any)
	var roles []string
	for _, m := range msgs {
		roles = append(roles, m.(map[string]any)["role"].(string))
	}
	if strings.Join(roles, ",") != "system,user,assistant" {
		t.Fatalf("roles = %v", roles)
	}
	if got := msgs[0].(map[string]any)["content"]; got != "a\n\nb\n\nc" {
		t.Fatalf("merged = %q", got)
	}
}

func TestStripToolSchemaPattern(t *testing.T) {
	var req map[string]any
	_ = json.Unmarshal([]byte(`{"tools":[{"type":"function","function":{"name":"Grep","parameters":{"type":"object","properties":{"pattern":{"type":"string","description":"regex"},"id":{"type":"string","pattern":"^[a-z]+$"}},"items":{"pattern":"x"}}}}]}`), &req)
	stripUnsupportedToolSchemaKeywords(req)
	b, _ := json.Marshal(req)
	s := string(b)
	if strings.Contains(s, `"^[a-z]+$"`) || strings.Contains(s, `"pattern":"x"`) {
		t.Fatalf("pattern keyword not stripped: %s", s)
	}
	if !strings.Contains(s, `"pattern":{"description":"regex","type":"string"}`) {
		t.Fatalf("property named pattern was removed: %s", s)
	}
}
