package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

type sessionCtxKey struct{}

// OpenCode Go routes and caches by session; it requires x-opencode-session.
// Clients send their own session header under different names.
var clientSessionHeaders = []string{
	"X-Opencode-Session",
	"Session_id",
	"Session-Id",
	"X-Session-Id",
	"X-Codex-Session-Id",
	"X-Claude-Code-Session-Id",
	"X-Client-Request-Id",
}

var processSessionID = randomSessionID()

func randomSessionID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return "ocgo-" + hex.EncodeToString(b)
}

func sessionIDFromRequest(r *http.Request) string {
	for _, h := range clientSessionHeaders {
		if v := strings.TrimSpace(r.Header.Get(h)); v != "" {
			return v
		}
	}
	if r.Body != nil && r.Method == http.MethodPost {
		if body, err := io.ReadAll(r.Body); err == nil {
			r.Body = io.NopCloser(bytes.NewReader(body))
			if id := sessionIDFromBody(body); id != "" {
				return id
			}
		}
	}
	return processSessionID
}

// sessionIDFromBody derives a stable per-conversation ID from fields clients
// put in the body: Responses "prompt_cache_key" or Anthropic "metadata.user_id"
// (Claude Code embeds its session there).
func sessionIDFromBody(body []byte) string {
	var req struct {
		PromptCacheKey string `json:"prompt_cache_key"`
		Metadata       struct {
			UserID string `json:"user_id"`
		} `json:"metadata"`
	}
	if json.Unmarshal(body, &req) != nil {
		return ""
	}
	key := req.PromptCacheKey
	if key == "" {
		key = req.Metadata.UserID
	}
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return "ocgo-" + hex.EncodeToString(sum[:8])
}

// withSession stores the client's session ID in the request context so every
// upstream request built from it can forward the session.
func withSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), sessionCtxKey{}, sessionIDFromRequest(r))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// setUpstreamHeaders adds the OpenCode Go session and user agent headers.
func setUpstreamHeaders(req *http.Request) {
	id, _ := req.Context().Value(sessionCtxKey{}).(string)
	if id == "" {
		id = processSessionID
	}
	req.Header.Set("x-opencode-session", id)
	req.Header.Set("User-Agent", "ocgo/"+version)
}
