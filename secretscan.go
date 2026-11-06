package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// ─── secret scanner (Gate 1: write-time) ────────────────────────────────────
// Each corpus uses its own allowlisted rules file, shared with compile.py Gate 2.
// RE2-only patterns so Go and Python redact identically.

type SecretPattern struct {
	Name         string  `json:"name"`
	Regex        string  `json:"regex"`
	Placeholder  string  `json:"placeholder"`
	Action       string  `json:"action"` // "redact" | "log"
	CaptureGroup int     `json:"capture_group"`
	MinLen       int     `json:"min_len"`
	MinEntropy   float64 `json:"min_entropy"`
	re           *regexp.Regexp
}

type SecretRules struct {
	Version                int             `json:"version"`
	Patterns               []SecretPattern `json:"patterns"`
	QueryRedactionPatterns []SecretPattern `json:"query_redaction_patterns"`
	Allowlist              []string        `json:"allowlist"`
	AllowContain           []string        `json:"allow_contains"`
	allow                  map[string]struct{}
}

type scanHit struct {
	Pattern string
	Action  string
}

func sanitizeWrite(profile CorpusProfile, content, title, tags string) (string, string, string, []scanHit, error) {
	rules, err := loadSecretRules(profile.SecretPatternsPath)
	if err != nil {
		return "", "", "", nil, err
	}
	cleanedContent, hits := rules.Sanitize(content)
	cleanedTitle := title
	if title != "" {
		var titleHits []scanHit
		cleanedTitle, titleHits = rules.Sanitize(title)
		// A secret in the title must reach the caller: the hits drive the warning
		// and the .orig backup, exactly as a secret in the body does.
		hits = append(hits, titleHits...)
	}
	cleanedTags := tags
	if tags != "" {
		var tagHits []scanHit
		cleanedTags, tagHits = rules.Sanitize(tags)
		hits = append(hits, tagHits...)
	}
	return cleanedContent, cleanedTitle, cleanedTags, hits, nil
}


func loadSecretRules(path string) (*SecretRules, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r SecretRules
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if r.Version != 1 {
		return nil, fmt.Errorf("parse %s: unsupported secret rules version %d", path, r.Version)
	}
	if len(r.Patterns) == 0 {
		return nil, fmt.Errorf("parse %s: secret rules must contain at least one pattern", path)
	}
	for i := range r.Patterns {
		pattern := &r.Patterns[i]
		if pattern.Name == "" || pattern.Regex == "" {
			return nil, fmt.Errorf("parse %s: pattern %d requires name and regex", path, i)
		}
		if pattern.Action != "redact" && pattern.Action != "log" {
			return nil, fmt.Errorf("parse %s: pattern %q has invalid action %q", path, pattern.Name, pattern.Action)
		}
		if pattern.Action == "redact" && pattern.Placeholder == "" {
			return nil, fmt.Errorf("parse %s: redact pattern %q requires placeholder", path, pattern.Name)
		}
		if pattern.CaptureGroup < 0 {
			return nil, fmt.Errorf("parse %s: pattern %q has negative capture_group", path, pattern.Name)
		}
		re, err := regexp.Compile(r.Patterns[i].Regex)
		if err != nil {
			return nil, fmt.Errorf("pattern %q: %w", r.Patterns[i].Name, err)
		}
		if pattern.CaptureGroup > re.NumSubexp() {
			return nil, fmt.Errorf(
				"parse %s: pattern %q capture_group %d exceeds %d groups",
				path, pattern.Name, pattern.CaptureGroup, re.NumSubexp(),
			)
		}
		r.Patterns[i].re = re
	}
	r.allow = make(map[string]struct{})
	for _, a := range r.Allowlist {
		clean := asciiToLower(asciiTrimSpace(a))
		if clean != "" {
			r.allow[clean] = struct{}{}
		}
	}
	for _, a := range r.AllowContain {
		clean := asciiToLower(asciiTrimSpace(a))
		if clean != "" {
			r.allow[clean] = struct{}{}
		}
	}
	for _, p := range r.Patterns {
		clean := asciiToLower(asciiTrimSpace(p.Placeholder))
		if clean != "" {
			r.allow[clean] = struct{}{}
		}
	}
	for _, p := range r.QueryRedactionPatterns {
		clean := asciiToLower(asciiTrimSpace(p.Placeholder))
		if clean != "" {
			r.allow[clean] = struct{}{}
		}
	}
	return &r, nil
}

func isASCIIWhitespace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\v' || b == '\f'
}

func asciiTrimSpace(s string) string {
	start := 0
	for start < len(s) && isASCIIWhitespace(s[start]) {
		start++
	}
	end := len(s)
	for end > start && isASCIIWhitespace(s[end-1]) {
		end--
	}
	return s[start:end]
}

func asciiToLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// allowed reports whether a captured value matches an exact allowed literal:
// an entry in allowlist, allow_contains, or a placeholder marker. Comparison is
// whole-value exact match after trimming ASCII whitespace and lowercasing ASCII.
func (r *SecretRules) allowed(value string) bool {
	v := asciiToLower(asciiTrimSpace(value))
	if v == "" {
		return true
	}
	_, ok := r.allow[v]
	return ok
}

func isOwnMarkerContinuation(value, placeholder string) bool {
	v := asciiToLower(asciiTrimSpace(value))
	marker := asciiToLower(placeholder)
	return v == marker || strings.HasPrefix(v, marker+" ") || strings.HasPrefix(v, marker+"\t")
}

// shannonEntropy returns bits-per-character Shannon entropy over the bytes of s.
// ASCII-only secrets make this identical to the Python side.
func shannonEntropy(s string) float64 {
	if s == "" {
		return 0
	}
	var freq [256]int
	for i := 0; i < len(s); i++ {
		freq[s[i]]++
	}
	n := float64(len(s))
	h := 0.0
	for _, c := range freq {
		if c == 0 {
			continue
		}
		p := float64(c) / n
		h -= p * math.Log2(p)
	}
	return h
}

// Sanitize scans content, redacting matches whose pattern action is "redact"
// and returning every surviving hit (redact and log). Patterns run in order;
// once a value is redacted, later patterns see the <REDACTED_…> marker and skip
// it via the allowlist.
func (r *SecretRules) Sanitize(content string) (string, []scanHit) {
	var hits []scanHit
	loggedPatterns := make(map[string]bool)
	for _, p := range r.Patterns {
		matches := p.re.FindAllStringSubmatchIndex(content, -1)
		if matches == nil {
			continue
		}
		// Walk matches in reverse so replacement offsets stay valid.
		for i := len(matches) - 1; i >= 0; i-- {
			m := matches[i]
			gi := p.CaptureGroup * 2
			if gi+1 >= len(m) || m[gi] < 0 {
				continue
			}
			start, end := m[gi], m[gi+1]
			value := content[start:end]
			if p.Action == "redact" && isOwnMarkerContinuation(value, p.Placeholder) {
				continue
			}

			if r.allowed(value) {
				if p.Action == "redact" && !strings.HasPrefix(asciiToLower(value), "<redacted_") {
					if !loggedPatterns[p.Name] {
						log.Printf("secret scanner: allow-set skipped pattern %s", p.Name)
						loggedPatterns[p.Name] = true
					}
				}
				continue
			}
			if utf8.RuneCountInString(value) < p.MinLen {
				continue
			}
			if p.MinEntropy > 0 && shannonEntropy(value) < p.MinEntropy {
				continue
			}

			hits = append(hits, scanHit{Pattern: p.Name, Action: p.Action})
			if p.Action == "redact" {
				content = content[:start] + p.Placeholder + content[end:]
			}
		}
	}
	return content, hits
}

// recordQuarantine appends one audit line with metadata only to the quarantine log.
// No pre-redaction plaintext is written; no review queue or notification — just a trail.
func recordQuarantine(profile CorpusProfile, source, ref string, hits []scanHit) {
	ts := time.Now().Format("2006-01-02T15:04:05")
	names := make([]string, 0, len(hits))
	for _, h := range hits {
		names = append(names, h.Pattern+":"+h.Action)
	}

	corpusRoot := filepath.Dir(profile.QuarantineDir)
	line := fmt.Sprintf("%s id=%s source=%s hits=%s\n",
		ts, ref, source, strings.Join(names, ","))
	if f, err := os.OpenFile(profile.QuarantineLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600); err != nil {
		fmt.Fprintf(os.Stderr, "warn: quarantine log unavailable (%s): %v — this redaction is not audited\n",
			profile.QuarantineLog, err)
	} else {
		if _, err := f.WriteString(line); err != nil {
			fmt.Fprintf(os.Stderr, "warn: quarantine log write failed (%s): %v\n", profile.QuarantineLog, err)
		}
		if err := f.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "warn: quarantine log close failed (%s): %v\n", profile.QuarantineLog, err)
		}
		if err := alignOwnerWithDir(corpusRoot, profile.QuarantineLog); err != nil {
			fmt.Fprintf(os.Stderr, "warn: could not align owner of %s with %s: %v\n", profile.QuarantineLog, corpusRoot, err)
		}
	}
}
