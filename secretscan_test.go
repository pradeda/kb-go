package main

import (
	"strings"
	"testing"
)

func mustRules(t *testing.T) *SecretRules {
	t.Helper()
	r, err := loadSecretRules(secretPatternsPath)
	if err != nil {
		t.Skipf("secret rules not available (%v)", err)
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
		"password: your_password_here",
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
