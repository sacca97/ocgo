package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
)

// Local models (e.g. a llama.cpp `llama-server`) are exposed as "local/<id>".
// They are routed purely by this prefix so a routing decision never needs a
// network call, and they can never collide with OpenCode or Codex model IDs.
const localPrefix = "local/"

const defaultLocalURL = "http://127.0.0.1:8080"

// defaultLocalContextWindow is used when the server does not report its
// context size. It is deliberately conservative: an overestimate stops Codex
// from compacting before the server overflows.
const defaultLocalContextWindow = 32768

// LocalConfig describes an OpenAI-compatible local server such as llama-server.
type LocalConfig struct {
	URL           string `json:"url"`
	APIKey        string `json:"api_key,omitempty"`
	ContextWindow int    `json:"context_window,omitempty"`
	// CACert is a PEM file to trust, for servers using a self-signed or
	// private-CA certificate. Insecure skips certificate verification entirely.
	CACert   string `json:"ca_cert,omitempty"`
	Insecure bool   `json:"insecure,omitempty"`
}

func isLocalModel(model string) bool {
	return strings.HasPrefix(strings.TrimSpace(model), localPrefix)
}

func localModelID(model string) string {
	return strings.TrimPrefix(strings.TrimSpace(model), localPrefix)
}

// localBase returns the server root without a trailing slash or "/v1".
func (c LocalConfig) localBase() string {
	u := strings.TrimRight(strings.TrimSpace(c.URL), "/")
	u = strings.TrimSuffix(u, "/v1")
	if u == "" {
		u = defaultLocalURL
	}
	return u
}

func (c LocalConfig) chatURL() string { return c.localBase() + "/v1/chat/completions" }

type localInfo struct {
	Models        []string // IDs without the "local/" prefix
	ContextWindow int      // 0 when unknown
}

var localClients sync.Map // LocalConfig -> *http.Client

// localHTTPClient returns an HTTP client that trusts the configured CA (or skips
// verification) for the local server. Clients are cached so connections are reused.
func localHTTPClient(c LocalConfig) (*http.Client, error) {
	if v, ok := localClients.Load(c); ok {
		return v.(*http.Client), nil
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tlsCfg := &tls.Config{InsecureSkipVerify: c.Insecure}
	if c.CACert != "" {
		pem, err := os.ReadFile(c.CACert)
		if err != nil {
			return nil, fmt.Errorf("reading local CA certificate: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("no certificates found in %s", c.CACert)
		}
		tlsCfg.RootCAs = pool
	}
	tr.TLSClientConfig = tlsCfg
	client := &http.Client{Transport: errorLoggingTransport{tr}, Timeout: 10 * time.Minute}
	localClients.Store(c, client)
	return client, nil
}

func localGet(ctx context.Context, c LocalConfig, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.localBase()+path, nil)
	if err != nil {
		return err
	}
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	client, err := localHTTPClient(c)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s returned status %d", path, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	return json.NewDecoder(bytes.NewReader(b)).Decode(out)
}

// fetchLocalInfo queries the local server for its models and context size.
func fetchLocalInfo(c LocalConfig) (localInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var models struct {
		Data []struct {
			ID   string `json:"id"`
			Meta struct {
				NCtx int `json:"n_ctx"`
			} `json:"meta"`
		} `json:"data"`
	}
	if err := localGet(ctx, c, "/v1/models", &models); err != nil {
		return localInfo{}, err
	}
	var info localInfo
	for _, m := range models.Data {
		if id := strings.TrimSpace(m.ID); id != "" {
			info.Models = append(info.Models, id)
			if info.ContextWindow == 0 {
				info.ContextWindow = m.Meta.NCtx
			}
		}
	}
	// llama-server reports the runtime context size (-c) in /props; this is
	// best effort because the field layout differs between builds.
	var props struct {
		NCtx     int `json:"n_ctx"`
		Settings struct {
			NCtx int `json:"n_ctx"`
		} `json:"default_generation_settings"`
	}
	if localGet(ctx, c, "/props", &props) == nil {
		if props.Settings.NCtx > 0 {
			info.ContextWindow = props.Settings.NCtx
		} else if props.NCtx > 0 {
			info.ContextWindow = props.NCtx
		}
	}
	return info, nil
}

// loadLocalConfig returns the saved local server config, if any.
var loadLocalConfig = func() *LocalConfig { return loadFileConfig().Local }

var localInfoFetcher = newLazyFetcher(func() (localInfo, error) {
	c := loadLocalConfig()
	if c == nil {
		return localInfo{}, errors.New("no local server configured")
	}
	return fetchLocalInfo(*c)
})

// localModelIDs lists the local models as "local/<id>". It is empty, without
// error, when no server is configured or it is not reachable.
func localModelIDs() []string {
	info, err := localInfoFetcher.get()
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(info.Models))
	for _, id := range info.Models {
		out = append(out, localPrefix+id)
	}
	return out
}

// allModelIDs is every model ocgo can serve: OpenCode Go plus local ones.
func allModelIDs() []string {
	return append(knownModelIDs(), localModelIDs()...)
}

func localContextWindow() int {
	if c := loadLocalConfig(); c != nil && c.ContextWindow > 0 {
		return c.ContextWindow
	}
	if info, err := localInfoFetcher.get(); err == nil && info.ContextWindow > 0 {
		return info.ContextWindow
	}
	return defaultLocalContextWindow
}

// newChatUpstreamRequest builds the upstream /chat/completions request for the
// model named in body. Local models go to the local server with the local key
// and the "local/" prefix stripped; everything else goes to OpenCode Go with
// the OpenCode key. This is the only place that picks a chat upstream, so the
// OpenCode key can never be sent to the local server.
func newChatUpstreamRequest(ctx context.Context, cfg Config, body []byte) (*http.Request, *http.Client, error) {
	var head struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &head)
	if !isLocalModel(head.Model) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, openAIURL, bytes.NewReader(body))
		if err != nil {
			return nil, nil, err
		}
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
		setUpstreamHeaders(req)
		req.Header.Set("Content-Type", "application/json")
		return req, &http.Client{Timeout: 10 * time.Minute}, nil
	}
	if cfg.Local == nil {
		return nil, nil, errors.New("local model requested but no local server is configured; run: ocgo local set")
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, nil, err
	}
	raw["model"] = localModelID(head.Model)
	mergeSystemMessages(raw)
	stripUnsupportedToolSchemaKeywords(raw)
	out, err := json.Marshal(raw)
	if err != nil {
		return nil, nil, err
	}
	client, err := localHTTPClient(*cfg.Local)
	if err != nil {
		return nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.Local.chatURL(), bytes.NewReader(out))
	if err != nil {
		return nil, nil, err
	}
	if cfg.Local.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Local.APIKey)
	}
	req.Header.Set("Content-Type", "application/json")
	return req, client, nil
}

func localCmd() *cobra.Command {
	var apiKey string
	var ctxWindow int
	var caCert string
	var insecure bool
	cmd := &cobra.Command{Use: "local", Short: "Use a local llama.cpp (llama-server) model through ocgo"}
	set := &cobra.Command{Use: "set [URL]", Short: "Save the local server (default " + defaultLocalURL + ")", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cfg := loadFileConfig()
		lc := LocalConfig{URL: defaultLocalURL, APIKey: apiKey, ContextWindow: ctxWindow, CACert: caCert, Insecure: insecure}
		if len(args) == 1 {
			lc.URL = args[0]
		}
		cfg.Local = &lc
		if err := saveConfig(cfg); err != nil {
			return err
		}
		fmt.Println("Restart a running proxy (ocgo stop) for the change to apply.")
		return printLocalStatus(lc)
	}}
	set.Flags().StringVar(&apiKey, "api-key", "", "API key if llama-server runs with --api-key")
	set.Flags().IntVar(&ctxWindow, "context", 0, "Context window in tokens, if the server does not report it")
	set.Flags().StringVar(&caCert, "ca-cert", "", "PEM file to trust for a self-signed or private-CA HTTPS server")
	set.Flags().BoolVar(&insecure, "insecure", false, "Skip TLS certificate verification (self-signed server)")
	show := &cobra.Command{Use: "show", Short: "Show the local server and its models", RunE: func(cmd *cobra.Command, args []string) error {
		lc := loadFileConfig().Local
		if lc == nil {
			fmt.Println("No local server configured; run: ocgo local set")
			return nil
		}
		return printLocalStatus(*lc)
	}}
	clear := &cobra.Command{Use: "clear", Short: "Remove the local server", RunE: func(cmd *cobra.Command, args []string) error {
		cfg := loadFileConfig()
		cfg.Local = nil
		return saveConfig(cfg)
	}}
	cmd.AddCommand(set, show, clear)
	return cmd
}

func printLocalStatus(lc LocalConfig) error {
	fmt.Printf("Local server: %s\n", lc.localBase())
	info, err := fetchLocalInfo(lc)
	if err != nil {
		fmt.Printf("  not reachable (%v); start llama-server, e.g. llama-server -m model.gguf --jinja -c 32768\n", err)
		return nil
	}
	if info.ContextWindow > 0 {
		fmt.Printf("  context window: %d\n", info.ContextWindow)
	} else {
		fmt.Printf("  context window: not reported (using %d; override with --context)\n", defaultLocalContextWindow)
	}
	for _, id := range info.Models {
		fmt.Printf("  %s%s\n", localPrefix, id)
	}
	return nil
}

// errorLoggingTransport logs error responses from the local server to the
// proxy log (~/.config/ocgo/ocgo.log). Clients truncate upstream errors, but
// llama-server's message (e.g. a chat-template failure) is the useful part.
type errorLoggingTransport struct{ next http.RoundTripper }

func (t errorLoggingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "local server %s: %v\n", req.URL.Path, err)
		return resp, err
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		fmt.Fprintf(os.Stderr, "local server %s returned %d: %s\n", req.URL.Path, resp.StatusCode, strings.TrimSpace(string(b)))
		saveFailedLocalRequest(req)
		resp.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(b), resp.Body), resp.Body}
	}
	return resp, nil
}

// saveFailedLocalRequest keeps the latest request that the local server
// rejected, so a template or schema failure can be replayed and inspected.
func saveFailedLocalRequest(req *http.Request) {
	if req.GetBody == nil {
		return
	}
	body, err := req.GetBody()
	if err != nil {
		return
	}
	defer body.Close()
	b, err := io.ReadAll(body)
	if err != nil {
		return
	}
	_ = os.MkdirAll(configDir(), 0755)
	path := filepath.Join(configDir(), "last-local-error-request.json")
	if os.WriteFile(path, b, 0600) == nil {
		fmt.Fprintf(os.Stderr, "saved the failing request to %s\n", path)
	}
}

// mergeSystemMessages folds every system/developer message into a single
// leading system message. Claude Code and Codex insert system or developer
// messages mid-conversation, which many chat templates (Qwen, ...) reject with
// "System message must be at the beginning".
func mergeSystemMessages(req map[string]any) {
	msgs, _ := req["messages"].([]any)
	var system []string
	rest := make([]any, 0, len(msgs))
	for _, m := range msgs {
		msg, ok := m.(map[string]any)
		role, _ := msg["role"].(string)
		if !ok || (role != "system" && role != "developer") {
			rest = append(rest, m)
			continue
		}
		text, ok := messageText(msg["content"])
		if !ok {
			rest = append(rest, m) // non-text content: leave untouched
			continue
		}
		if text != "" {
			system = append(system, text)
		}
	}
	if len(system) == 0 {
		return
	}
	merged := map[string]any{"role": "system", "content": strings.Join(system, "\n\n")}
	req["messages"] = append([]any{merged}, rest...)
}

// messageText returns the text of a string or text-parts content value.
func messageText(content any) (string, bool) {
	switch c := content.(type) {
	case string:
		return c, true
	case nil:
		return "", true
	case []any:
		var parts []string
		for _, p := range c {
			part, ok := p.(map[string]any)
			text, isText := part["text"].(string)
			if !ok || !isText {
				return "", false
			}
			parts = append(parts, text)
		}
		return strings.Join(parts, "\n"), true
	}
	return "", false
}

// stripUnsupportedToolSchemaKeywords removes JSON Schema keywords that
// llama.cpp cannot compile into a grammar (a regex "pattern" makes the whole
// request fail with "failed to parse grammar"). Property names are left alone,
// so a parameter called "pattern" survives.
func stripUnsupportedToolSchemaKeywords(req map[string]any) {
	tools, _ := req["tools"].([]any)
	for _, t := range tools {
		tool, _ := t.(map[string]any)
		fn, _ := tool["function"].(map[string]any)
		if params, ok := fn["parameters"]; ok {
			walkToolSchema(params)
		}
	}
}

func walkToolSchema(v any) {
	switch s := v.(type) {
	case map[string]any:
		if _, isString := s["pattern"].(string); isString {
			delete(s, "pattern")
		}
		for key, child := range s {
			if key == "properties" || key == "$defs" || key == "definitions" {
				// A map of name -> schema: recurse into the schemas only.
				if named, ok := child.(map[string]any); ok {
					for _, sub := range named {
						walkToolSchema(sub)
					}
				}
				continue
			}
			walkToolSchema(child)
		}
	case []any:
		for _, item := range s {
			walkToolSchema(item)
		}
	}
}
