package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSanitizeWriteFailsClosedWithoutCorpusRules(t *testing.T) {
	profile, err := corpusProfile("ai")
	if err != nil {
		t.Fatal(err)
	}
	profile.SecretPatternsPath = filepath.Join(t.TempDir(), "missing-rules.json")
	if _, _, _, _, err := sanitizeWrite(profile, "secret content", "title", "tags"); err == nil {
		t.Fatal("sanitizeWrite unexpectedly succeeded without corpus rules")
	}
}

func TestLoadSecretRulesRejectsSemanticallyEmptyConfig(t *testing.T) {
	for _, body := range []string{`{}`, `{"version":1,"patterns":[]}`} {
		path := filepath.Join(t.TempDir(), "rules.json")
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadSecretRules(path); err == nil {
			t.Fatalf("loadSecretRules unexpectedly accepted %s", body)
		}
	}
}

func mustRules(t *testing.T) *SecretRules {
	t.Helper()
	path := filepath.Join("config", "secret_patterns.json")
	r, err := loadSecretRules(path)
	if err != nil {
		t.Fatalf("load worktree secret rules from %s: %v", path, err)
	}
	return r
}

func TestSanitize_Tier1Redacts(t *testing.T) {
	r := mustRules(t)
	cases := []struct {
		name, in, wantMarker string
	}{
		{"openrouter", "key sk-or-v1-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef end", "<REDACTED_OPENROUTER_KEY>"},
		{"telegram", "bot 123456789:ABCDEFGHIJKLMNOPQRSTUVWXYZ012345678 ok", "<REDACTED_TELEGRAM_TOKEN>"},
		{"github", "token ghp_0123456789abcdefghijABCDEFGHIJ123456 done", "<REDACTED_GITHUB_TOKEN>"},
		{"anthropic", "sk-ant-api03-abcDEF0123456789xyz done", "<REDACTED_ANTHROPIC_KEY>"},
		{"aws", "id AKIA1234567890ABCDEF here", "<REDACTED_AWS_KEY>"},
		{"url_auth", "redis://user:s3cr3tPass@192.168.1.1:6379", "<REDACTED_URL_PASSWORD>"},
		{"arr_api", "api_key=0123456789abcdef0123456789abcdef", "<REDACTED_API_KEY>"},
		{"password_assign", "POSTGRES_PASSWORD=hunter2xyz", "<REDACTED_SECRET>"},
		{"ssh", "-----BEGIN OPENSSH PRIVATE KEY-----\nabc\ndef\n-----END OPENSSH PRIVATE KEY-----", "<REDACTED_SSH_PRIVATE_KEY>"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, hits := r.Sanitize(c.in)
			if !strings.Contains(out, c.wantMarker) {
				t.Errorf("Sanitize(%q) = %q, want marker %q", c.in, out, c.wantMarker)
			}
			if len(hits) == 0 {
				t.Errorf("Sanitize(%q) recorded no hits", c.in)
			}
		})
	}
}

func TestSanitize_URLKeepsUserHost(t *testing.T) {
	r := mustRules(t)
	out, _ := r.Sanitize("redis://user:s3cr3tPass@192.168.1.1:6379")
	if !strings.Contains(out, "user:") || !strings.Contains(out, "@192.168.1.1:6379") {
		t.Errorf("url auth should redact only password, got %q", out)
	}
	if strings.Contains(out, "s3cr3tPass") {
		t.Errorf("password leaked: %q", out)
	}
}

func TestSanitize_Allowlist(t *testing.T) {
	r := mustRules(t)
	cases := []string{
		"the SONARR_API_KEY variable is set elsewhere",
		"env var API_KEY documented here",
		"PASSWORD=changeme",
		"password: example",
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			out, hits := r.Sanitize(in)
			if out != in {
				t.Errorf("allowlisted input mutated: %q -> %q", in, out)
			}
			for _, h := range hits {
				if h.Action == "redact" {
					t.Errorf("allowlisted input produced redact hit: %+v", h)
				}
			}
		})
	}
}

func TestSanitize_ProseNotRedacted(t *testing.T) {
	// "secret"/"password" followed by a plain word (no : or =) is prose, not an
	// assignment — must not be redacted. Regression for the self-dogfood FP where
	// "KB secret scanner" got mangled to "KB secret <REDACTED_SECRET>".
	r := mustRules(t)
	cases := []string{
		"KB secret scanner implementation",
		"the secret sauce of this design",
		"password reset flow works fine",
		"this is a secret project nobody knows",
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			out, hits := r.Sanitize(in)
			if out != in {
				t.Errorf("prose mutated: %q -> %q", in, out)
			}
			for _, h := range hits {
				if h.Action == "redact" {
					t.Errorf("prose produced redact hit: %+v", h)
				}
			}
		})
	}
}

func TestSanitize_Idempotent(t *testing.T) {
	r := mustRules(t)
	once, _ := r.Sanitize("key sk-or-v1-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef x")
	twice, hits := r.Sanitize(once)
	if twice != once {
		t.Errorf("not idempotent: %q -> %q", once, twice)
	}
	for _, h := range hits {
		if h.Action == "redact" {
			t.Errorf("re-redacted already-clean content: %+v", h)
		}
	}
}

func TestSanitize_MultiHit(t *testing.T) {
	r := mustRules(t)
	in := "or sk-or-v1-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef and tg 123456789:ABCDEFGHIJKLMNOPQRSTUVWXYZ012345678"
	out, hits := r.Sanitize(in)
	if strings.Contains(out, "sk-or-v1-") || strings.Contains(out, "123456789:ABCDEF") {
		t.Errorf("multi-hit not fully redacted: %q", out)
	}
	if len(hits) < 2 {
		t.Errorf("want >=2 hits, got %d", len(hits))
	}
}

func TestSanitize_GenericIsLogOnly(t *testing.T) {
	r := mustRules(t)
	// High-entropy generic value: recorded but NOT mutated (action=log).
	in := "token=Xq9Rf2Lm7Zb4Vn8Kc1Dp6Ws3Ty0Ha5"
	out, hits := r.Sanitize(in)
	if out != in {
		t.Errorf("generic (log) hit mutated content: %q -> %q", in, out)
	}
	found := false
	for _, h := range hits {
		if h.Pattern == "generic_secret_value" && h.Action == "log" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected generic_secret_value log hit, got %+v", hits)
	}
}

func TestSanitize_LowEntropyGenericSkipped(t *testing.T) {
	r := mustRules(t)
	in := "token=aaaaaaaaaaaaaaaaaaaaaaaaaa"
	out, hits := r.Sanitize(in)
	if out != in {
		t.Errorf("low-entropy generic mutated: %q -> %q", in, out)
	}
	for _, h := range hits {
		if h.Pattern == "generic_secret_value" {
			t.Errorf("low-entropy value should be filtered, got hit %+v", h)
		}
	}
}

func TestShannonEntropy(t *testing.T) {
	if e := shannonEntropy("aaaaaaaa"); e != 0 {
		t.Errorf("uniform string entropy = %v, want 0", e)
	}
	if e := shannonEntropy("Xq9Rf2Lm7Zb4Vn8Kc1Dp6Ws3Ty0Ha5"); e < 4.0 {
		t.Errorf("random string entropy = %v, want >=4.0", e)
	}
}

// ─── Gate 1: the title is scanned with the same rules as the body ─────────────
//
// A secret that appears only in the title used to be redacted silently: the
// scanner dropped the title's hits, so `kb add` never warned, never quarantined
// the pre-redaction text and never wrote an .orig backup. Python Gate 2
// (compile.py sanitize_unembedded) counts title and summary hits, so the two
// gates disagreed about the same input.

func TestSanitizeWriteCountsTitleHits(t *testing.T) {
	profile, err := corpusProfile(defaultCorpus)
	if err != nil {
		t.Fatal(err)
	}
	const secretTitle = "POSTGRES_PASSWORD=hunter2xyz"

	cleanedContent, cleanedTitle, cleanedTags, hits, err := sanitizeWrite(profile, "clean body", secretTitle, "clean tags")
	if err != nil {
		t.Fatal(err)
	}
	if cleanedContent != "clean body" {
		t.Errorf("body mutated without a secret in it: %q", cleanedContent)
	}
	if cleanedTags != "clean tags" {
		t.Errorf("tags mutated without a secret in it: %q", cleanedTags)
	}
	if strings.Contains(cleanedTitle, "hunter2xyz") || !strings.Contains(cleanedTitle, "<REDACTED_SECRET>") {
		t.Errorf("title not redacted: %q", cleanedTitle)
	}
	if len(hits) == 0 {
		t.Fatal("title-only secret produced no hit: kb add would stay silent")
	}
}

func TestSanitizeWriteCountsBodyAndTitleHits(t *testing.T) {
	profile, err := corpusProfile(defaultCorpus)
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, hits, err := sanitizeWrite(profile, "token=0123456789abcdef0123456789abcdef", "POSTGRES_PASSWORD=hunter2xyz", "clean tags")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) < 2 {
		t.Fatalf("hits = %+v, want a hit for the body and the title", hits)
	}
}

func TestSanitizeWriteRedactsTags(t *testing.T) {
	profile, err := corpusProfile(defaultCorpus)
	if err != nil {
		t.Fatal(err)
	}
	const secretTags = "homelab,POSTGRES_PASSWORD=hunter2xyz,ops"
	cleanedContent, cleanedTitle, cleanedTags, hits, err := sanitizeWrite(profile, "clean body", "clean title", secretTags)
	if err != nil {
		t.Fatal(err)
	}
	if cleanedContent != "clean body" {
		t.Errorf("body mutated without a secret in it: %q", cleanedContent)
	}
	if cleanedTitle != "clean title" {
		t.Errorf("title mutated without a secret in it: %q", cleanedTitle)
	}
	if strings.Contains(cleanedTags, "hunter2xyz") || !strings.Contains(cleanedTags, "<REDACTED_SECRET>") {
		t.Errorf("tags not redacted: %q", cleanedTags)
	}
	if len(hits) == 0 {
		t.Fatal("tag-only secret produced no hit: kb add would stay silent")
	}
}

func TestAllowedExactMatchOnly(t *testing.T) {
	r := mustRules(t)

	keep := []string{
		"example",
		"changeme",
		"<REDACTED_SECRET>",
		"   example   ",
		"CHANGEME",
		"",
	}
	for _, val := range keep {
		if !r.allowed(val) {
			t.Errorf("r.allowed(%q) = false, want true (keep)", val)
		}
	}

	redact := []string{
		"your_password_here",
		"<REDACTED_SECRET> leftover",
		"hunter2example",
		"sk-or-v1-abcexample",
		"notreal123",
		"correct horse example battery",
		"managed by example script",
	}
	for _, val := range redact {
		if r.allowed(val) {
			t.Errorf("r.allowed(%q) = true, want false (redact)", val)
		}
	}
}

func TestRescanRedactedOutputUnchangedForEveryMarker(t *testing.T) {
	r := mustRules(t)
	markers := []string{
		"<REDACTED_SECRET>",
		"<REDACTED_API_KEY>",
		"<REDACTED_BESZEL_TOKEN>",
		"<REDACTED_URL_PASSWORD>",
		"<REDACTED_BASIC_AUTH>",
		"<REDACTED_TELEGRAM_TOKEN>",
	}
	for _, marker := range markers {
		input := "password: " + marker
		out, hits := r.Sanitize(input)
		if out != input {
			t.Errorf("re-scanning marker mutated: in=%q out=%q", input, out)
		}
		if len(hits) != 0 {
			t.Errorf("re-scanning marker produced hits: in=%q hits=%+v", input, hits)
		}
	}
}

func TestSecretAssignmentMultiWordCases(t *testing.T) {
	r := mustRules(t)
	cases := []struct {
		input       string
		want        string
		description string
	}{
		{"password: hunter2 extra words", "password: <REDACTED_SECRET>", "multi-word assignment"},
		{"password: abc def", "password: <REDACTED_SECRET>", "two short words exceeding 6 chars"},
		{`password: "two words here"`, `password: "<REDACTED_SECRET>"`, "quoted two words"},
		{"password: rotated monthly", "password: <REDACTED_SECRET>", "accepted cost: looks like secret"},
		{"PASSWORD=abc123 OTHER_SETTING=value", "PASSWORD=<REDACTED_SECRET>", "accepted cost: sibling setting included"},
		{"FOO_PASSWORD=hunter2xyz", "FOO_PASSWORD=<REDACTED_SECRET>", "prefix keyword without word boundary"},
		{"password:\nnext line", "password:\nnext line", "must not capture next line across newline"},
		{"password:\r\nnext line", "password:\r\nnext line", "must not capture next line across CRLF"},
		{"password: abc", "password: abc", "under 6 chars stays unredacted"},
	}
	for _, tc := range cases {
		t.Run(tc.description, func(t *testing.T) {
			out, _ := r.Sanitize(tc.input)
			if out != tc.want {
				t.Errorf("Sanitize(%q) = %q, want %q", tc.input, out, tc.want)
			}
		})
	}
}

func TestNonASCIILengthMeasureParity(t *testing.T) {
	r := mustRules(t)
	// 3 Cyrillic characters (6 bytes UTF-8) -> rune count 3 < min_len 6 -> must stay unredacted
	underSix := "password: пар"
	out, hits := r.Sanitize(underSix)
	if out != underSix || len(hits) != 0 {
		t.Errorf("3 non-ASCII characters (6 bytes) should be under min_len 6: out=%q hits=%+v", out, hits)
	}

	// 6 Cyrillic characters (12 bytes UTF-8) -> rune count 6 == min_len 6 -> must be redacted
	sixChars := "password: пароль"
	out, hits = r.Sanitize(sixChars)
	if out != "password: <REDACTED_SECRET>" || len(hits) == 0 {
		t.Errorf("6 non-ASCII characters should match min_len 6: out=%q hits=%+v", out, hits)
	}
}

func TestSecretPatternsParity(t *testing.T) {
	r := mustRules(t)
	data, err := os.ReadFile(filepath.Join("tests", "secret_patterns_parity.json"))
	if err != nil {
		t.Fatalf("read parity test cases: %v", err)
	}
	var cases struct {
		Positives []struct {
			Name       string `json:"name"`
			Pattern    string `json:"pattern"`
			Input      string `json:"input"`
			WantMarker string `json:"want_marker"`
			Captured   string `json:"captured"`
		} `json:"positives"`
		Negatives []struct {
			Name    string `json:"name"`
			Pattern string `json:"pattern"`
			Input   string `json:"input"`
		} `json:"negatives"`
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatalf("unmarshal parity test cases: %v", err)
	}

	for _, tc := range cases.Positives {
		t.Run(tc.Name, func(t *testing.T) {
			wantPattern := tc.Pattern
			if wantPattern == "" {
				wantPattern = "beszel_token"
			}
			out, hits := r.Sanitize(tc.Input)
			if !strings.Contains(out, tc.WantMarker) {
				t.Errorf("Sanitize(%q) = %q, want marker %q", tc.Input, out, tc.WantMarker)
			}
			found := false
			for _, h := range hits {
				if h.Pattern == wantPattern {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("Sanitize(%q) hits = %+v, want %s", tc.Input, hits, wantPattern)
			}
			if tc.Captured != "" {
				var captured string
				for _, p := range r.Patterns {
					if p.Name == wantPattern {
						sub := p.re.FindStringSubmatch(tc.Input)
						if len(sub) > p.CaptureGroup {
							captured = sub[p.CaptureGroup]
						}
						break
					}
				}
				if captured != tc.Captured {
					t.Errorf("captured = %q, want %q", captured, tc.Captured)
				}
			}
		})
	}

	for _, tc := range cases.Negatives {
		t.Run(tc.Name, func(t *testing.T) {
			wantPattern := tc.Pattern
			if wantPattern == "" {
				wantPattern = "beszel_token"
			}
			out, hits := r.Sanitize(tc.Input)
			if out != tc.Input {
				t.Errorf("negative test %q mutated: %q -> %q", tc.Name, tc.Input, out)
			}
			for _, h := range hits {
				if h.Pattern == wantPattern {
					t.Errorf("negative test %q unexpectedly hit %s: %+v", tc.Name, wantPattern, h)
				}
			}
		})
	}
}

// captureStderr runs fn with os.Stderr redirected to a pipe and returns everything
// written to it. recordQuarantine reports its failures on stderr, so this is how a
// swallowed failure is told apart from a reported one.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stderr
	os.Stderr = writer
	done := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(reader)
		done <- string(data)
	}()
	fn()
	os.Stderr = original
	writer.Close()
	out := <-done
	reader.Close()
	return out
}

// Every failure on the quarantine path has to be visible. Before this, an
// unusable quarantine directory or audit log produced an empty stderr and an
// empty backup path, so `kb add` looked like it had saved the original when it
// had not.
func TestQuarantineFailuresAreReported(t *testing.T) {
	hits := []scanHit{{Pattern: "password_assign", Action: "redact"}}

	t.Run("audit log cannot be opened", func(t *testing.T) {
		root := t.TempDir()
		blocker := filepath.Join(root, "blocker")
		if err := os.WriteFile(blocker, []byte("not a directory"), 0600); err != nil {
			t.Fatal(err)
		}
		profile := CorpusProfile{
			QuarantineDir: filepath.Join(root, "quarantine"),
			QuarantineLog: filepath.Join(blocker, "quarantine.log"),
		}
		stderr := captureStderr(t, func() {
			recordQuarantine(profile, "cli", "1", hits)
		})
		if !strings.Contains(stderr, "quarantine log") {
			t.Errorf("stderr = %q, want a visible warning about the quarantine log", stderr)
		}
	})
}

// Multiple `kb add` redactions append audit lines without secret values or backup paths.
func TestQuarantineAuditLinesRecordMetadataOnly(t *testing.T) {
	corpusRoot := t.TempDir()
	profile := CorpusProfile{
		QuarantineDir: filepath.Join(corpusRoot, "quarantine"),
		QuarantineLog: filepath.Join(corpusRoot, "quarantine.log"),
	}
	hits := []scanHit{{Pattern: "password_assign", Action: "redact"}}

	recordQuarantine(profile, "cli", "101", hits)
	recordQuarantine(profile, "cli", "102", hits)

	// Assert no quarantine directory or .orig backup files exist
	if entries, err := filepath.Glob(filepath.Join(profile.QuarantineDir, "*.orig")); err == nil && len(entries) > 0 {
		t.Fatalf("found unexpected quarantine backups: %v", entries)
	}

	data, err := os.ReadFile(profile.QuarantineLog)
	if err != nil {
		t.Fatalf("read quarantine log: %v", err)
	}
	logStr := string(data)
	if !strings.Contains(logStr, "id=101 source=cli hits=password_assign:redact") {
		t.Errorf("log missing entry 101: %q", logStr)
	}
	if !strings.Contains(logStr, "id=102 source=cli hits=password_assign:redact") {
		t.Errorf("log missing entry 102: %q", logStr)
	}
	if strings.Contains(logStr, "backup=") {
		t.Errorf("log contains backup=: %q", logStr)
	}
	if strings.Contains(logStr, "hunter2") {
		t.Errorf("log contains secret: %q", logStr)
	}
}
