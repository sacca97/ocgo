package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSelectModels(t *testing.T) {
	all := []string{"deepseek-v4-pro", "glm-5.1", "glm-5.3", "glm-5.3-flash", "kimi-2.6", "kimi-2.7", "kimi-2.7-thinking", "kimi-k2.6"}
	tests := []struct {
		name     string
		patterns []string
		want     []string
		err      string
	}{
		{"none", nil, all, ""},
		{"exact", []string{"kimi-2.6"}, []string{"kimi-2.6"}, ""},
		{"wildcard", []string{"kimi-2*"}, []string{"kimi-2.6", "kimi-2.7", "kimi-2.7-thinking"}, ""},
		{"multi", []string{"kimi-2*", "glm-5*"}, []string{"glm-5.1", "glm-5.3", "glm-5.3-flash", "kimi-2.6", "kimi-2.7", "kimi-2.7-thinking"}, ""},
		{"comma and space", []string{"kimi-2.6, glm-5.1"}, []string{"glm-5.1", "kimi-2.6"}, ""},
		{"exact plus regex", []string{"deepseek-v4-pro", "kimi-2*"}, []string{"deepseek-v4-pro", "kimi-2.6", "kimi-2.7", "kimi-2.7-thinking"}, ""},
		{"exclusion", []string{"kimi-2*", "!*thinking*"}, []string{"kimi-2.6", "kimi-2.7"}, ""},
		{"dedup", []string{"kimi-2*", "kimi-2.6", "kimi-2.6"}, []string{"kimi-2.6", "kimi-2.7", "kimi-2.7-thinking"}, ""},
		{"prefix", []string{"glm-5.3*"}, []string{"glm-5.3", "glm-5.3-flash"}, ""},
		{"glob question", []string{"glm-?.3"}, []string{"glm-5.3"}, ""},
		{"slash", []string{"*qwen*"}, nil, "matched no models"},
		{"whole id match", []string{"kimi"}, nil, "matched no models"},
		{"invalid pattern", []string{"[kimi"}, nil, "invalid model pattern"},
		{"no match", []string{"foo-model-999*"}, nil, "matched no models"},
		{"empty", []string{"kimi-*", "!kimi-*"}, nil, "empty catalog"},
		{"only exclusion", []string{"!kimi*"}, []string{"deepseek-v4-pro", "glm-5.1", "glm-5.3", "glm-5.3-flash"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := selectModels(all, tt.patterns)
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("err = %v, want %q", err, tt.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestWriteCodexModelCatalogSelected(t *testing.T) {
	withTempModelMappingFile(t, filepath.Join(t.TempDir(), "model-mapping.json"))
	if err := os.WriteFile(modelMappingFile(), []byte(`{"codex":{"gpt-5":"kimi-k2.6","gpt-5.5":"deepseek-v4-pro"}}`), 0644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "models.json")
	if err := writeCodexModelCatalog(path, []string{"kimi-k2.6"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	var catalog struct {
		Models []struct {
			Slug string `json:"slug"`
		} `json:"models"`
	}
	if err := json.Unmarshal(b, &catalog); err != nil {
		t.Fatal(err)
	}
	var slugs []string
	for _, m := range catalog.Models {
		slugs = append(slugs, m.Slug)
	}
	if want := []string{"kimi-k2.6", "gpt-5"}; !reflect.DeepEqual(slugs, want) {
		t.Fatalf("slugs = %v, want %v", slugs, want)
	}
}
