package main

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var errEmptyModelSelection = errors.New("model selection produced an empty catalog")

// normalizeModelPatterns splits comma-separated entries, trims whitespace and
// drops empty and duplicate patterns while preserving order.
func normalizeModelPatterns(raw []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, entry := range raw {
		for _, p := range strings.Split(entry, ",") {
			p = strings.TrimSpace(p)
			if p == "" || p == "!" || seen[p] {
				continue
			}
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

func matchModelPattern(all []string, pattern string) ([]string, error) {
	for _, id := range all {
		if id == pattern {
			return []string{id}, nil
		}
	}
	re, err := globRegexp(pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid model pattern %q: %w", pattern, err)
	}
	var out []string
	for _, id := range all {
		if re.MatchString(id) {
			out = append(out, id)
		}
	}
	return out, nil
}

// globRegexp converts a shell-style wildcard to an anchored regexp. Unlike
// path.Match, "*" also matches "/", so "*qwen*" finds "local/qwen3".
func globRegexp(pattern string) (*regexp.Regexp, error) {
	var sb strings.Builder
	sb.WriteString("^")
	rs := []rune(pattern)
	for i := 0; i < len(rs); i++ {
		switch c := rs[i]; c {
		case '*':
			sb.WriteString(".*")
		case '?':
			sb.WriteString(".")
		case '[':
			j := i + 1
			if j < len(rs) && (rs[j] == '!' || rs[j] == '^') {
				j++
			}
			if j < len(rs) && rs[j] == ']' {
				j++
			}
			for j < len(rs) && rs[j] != ']' {
				j++
			}
			if j >= len(rs) {
				return nil, errors.New("missing closing ]")
			}
			class := string(rs[i+1 : j])
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			sb.WriteString("[" + strings.ReplaceAll(class, `\`, `\\`) + "]")
			i = j
		default:
			sb.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	sb.WriteString("$")
	return regexp.Compile(sb.String())
}

// selectModels filters the known model IDs by exact IDs and shell-style
// wildcards (*, ?, [abc]). Patterns prefixed with "!" exclude models after inclusion. The
// result keeps the order of all and has no duplicates. With no patterns, all is
// returned unchanged.
func selectModels(all []string, patterns []string) ([]string, error) {
	patterns = normalizeModelPatterns(patterns)
	if len(patterns) == 0 {
		return all, nil
	}
	included := map[string]bool{}
	var excludes []string
	hasInclude := false
	for _, p := range patterns {
		if strings.HasPrefix(p, "!") {
			excludes = append(excludes, strings.TrimPrefix(p, "!"))
			continue
		}
		hasInclude = true
		matched, err := matchModelPattern(all, p)
		if err != nil {
			return nil, err
		}
		if len(matched) == 0 {
			if strings.Contains(p, ".*") {
				return nil, fmt.Errorf("model pattern %q matched no models (patterns are shell-style wildcards, not regexes; did you mean %q?)", p, strings.ReplaceAll(p, ".*", "*"))
			}
			return nil, fmt.Errorf("model pattern %q matched no models", p)
		}
		for _, id := range matched {
			included[id] = true
		}
	}
	if !hasInclude {
		// Only exclusions given: start from the full catalog.
		for _, id := range all {
			included[id] = true
		}
	}
	excluded := map[string]bool{}
	for _, p := range excludes {
		matched, err := matchModelPattern(all, p)
		if err != nil {
			return nil, err
		}
		for _, id := range matched {
			excluded[id] = true
		}
	}
	var out []string
	for _, id := range all {
		if included[id] && !excluded[id] {
			out = append(out, id)
		}
	}
	if len(out) == 0 {
		return nil, errEmptyModelSelection
	}
	return out, nil
}
