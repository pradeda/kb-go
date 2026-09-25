package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
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
	Version      int             `json:"version"`
	Patterns     []SecretPattern `json:"patterns"`
	Allowlist    []string        `json:"allowlist"`
	AllowContain []string        `json:"allow_contains"`
	allow        []string        // lowercased exact-match literals
	allowSub     []string        // lowercased substring needles
}

type scanHit struct {
	Pattern string
	Action  string
}

func sanitizeWrite(profile CorpusProfile, content, title string) (string, string, []scanHit, error) {
	rules, err := loadSecretRules(profile.SecretPatternsPath)
	if err != nil {
		return "", "", nil, err
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
	return cleanedContent, cleanedTitle, hits, nil
}

// quarantinePayload renders the pre-redaction record for the .orig backup: the
// original content, prefixed with the original title when the scanner mutated the
// title too. Without this the title's original text would be lost - the raw file
// and SQLite both hold the redacted title.
//
// compile.py Gate 2 renders the same shape (_quarantine_payload) for the fields it
// has: title, summary, content.
func quarantinePayload(origTitle, origContent, cleanedTitle, cleanedContent string) string {
	if origTitle != cleanedTitle {
		return fmt.Sprintf("title: %s\n\n%s", origTitle, origContent)
	}
	return origContent
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
	for _, a := range r.Allowlist {
		r.allow = append(r.allow, strings.ToLower(a))
	}
	for _, a := range r.AllowContain {
		r.allowSub = append(r.allowSub, strings.ToLower(a))
	}
	return &r, nil
}

// allowed reports whether a captured value is a known false-positive: an exact
// (case-insensitive) allowlist literal, or one that contains a placeholder
// needle (e.g. "your_password", "example", or a "<redacted_" marker).
func (r *SecretRules) allowed(value string) bool {
	v := strings.ToLower(strings.TrimSpace(value))
	if v == "" {
		return true
	}
	for _, a := range r.allow {
		if v == a {
			return true
		}
	}
	for _, a := range r.allowSub {
		if strings.Contains(v, a) {
			return true
		}
	}
	return false
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

			if r.allowed(value) {
				continue
			}
			if len(value) < p.MinLen {
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

// createQuarantineBackup writes the pre-redaction text to a file that did not
// exist before. The name is derived from the second plus the slug, and a collision
// (two adds in the same second with the same title) gets a numeric suffix instead
// of overwriting: an .orig that silently replaced an earlier one would destroy the
// only copy of that original while the audit log still claimed both were saved.
func createQuarantineBackup(dir, stem, content string) (string, error) {
	for attempt := 1; attempt <= 1000; attempt++ {
		name := stem + ".orig"
		if attempt > 1 {
			name = fmt.Sprintf("%s-%d.orig", stem, attempt)
		}
		path := filepath.Join(dir, name)
		handle, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		if _, err := handle.WriteString(content); err != nil {
			handle.Close()
			return "", err
		}
		if err := handle.Close(); err != nil {
			return "", err
		}
		return path, nil
	}
	return "", fmt.Errorf("no free quarantine backup name for %s", stem)
}

// recordQuarantine backs up the original content (when it was mutated) and
// appends one audit line. No queue, no notification — just a trail.
func recordQuarantine(profile CorpusProfile, orig, cleaned, source, slug string, hits []scanHit) (backupPath string) {
	ts := time.Now().Format("2006-01-02T15:04:05")
	names := make([]string, 0, len(hits))
	for _, h := range hits {
		names = append(names, h.Pattern+":"+h.Action)
	}

	// Every path created here gets the corpus owner, not root. A sudo-run
	// `kb add` that trips the scanner used to leave a root-owned 0600 .orig
	// behind, and the turok cron rsync in kb-backup.sh died on it (2026-08-11).
	corpusRoot := filepath.Dir(profile.QuarantineDir)

	if orig != cleaned {
		if err := os.MkdirAll(profile.QuarantineDir, 0700); err == nil {
			if err := alignOwnerWithDir(corpusRoot, profile.QuarantineDir); err != nil {
				fmt.Fprintf(os.Stderr, "warn: could not align owner of %s with %s: %v\n", profile.QuarantineDir, corpusRoot, err)
			}
			if path, backupErr := createQuarantineBackup(profile.QuarantineDir,
				fmt.Sprintf("%s-%s", time.Now().Format("20060102-150405"), slug), orig); backupErr != nil {
				fmt.Fprintf(os.Stderr, "warn: quarantine backup failed: %v\n", backupErr)
			} else {
				backupPath = path
				if err := alignOwnerWithDir(profile.QuarantineDir, backupPath); err != nil {
					fmt.Fprintf(os.Stderr, "warn: could not align owner of %s with %s: %v\n", backupPath, profile.QuarantineDir, err)
				}
			}
		} else {
			// Silent here would mean `kb add` claims to have kept the original
			// while it kept nothing (compile.py Gate 2 warns for the same case).
			fmt.Fprintf(os.Stderr, "warn: quarantine directory unavailable (%s): %v — the pre-redaction original is NOT saved\n",
				profile.QuarantineDir, err)
		}
	}

	line := fmt.Sprintf("%s source=%s hits=%s backup=%s\n",
		ts, source, strings.Join(names, ","), backupPath)
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
	return backupPath
}
