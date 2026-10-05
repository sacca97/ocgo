package main

import (
	"context"
	"encoding/json"
	"os/exec"
	"time"
)

// loadBundledCodexModels is a variable so tests can stub the codex binary.
var loadBundledCodexModels = bundledCodexModels

// bundledCodexModels returns the model catalog shipped with the installed
// Codex binary, so the normal OpenAI models stay in the /model picker.
func bundledCodexModels() []json.RawMessage {
	bin, err := exec.LookPath("codex")
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "debug", "models", "--bundled").Output()
	if err != nil {
		return nil
	}
	var catalog struct {
		Models []json.RawMessage `json:"models"`
	}
	if json.Unmarshal(out, &catalog) != nil {
		return nil
	}
	return catalog.Models
}
