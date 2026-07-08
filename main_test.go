package main

import (
	"os"
	"testing"
)

func TestSlugify(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"Docker tip", "docker-tip"},
		{"Šta je Docker", "sta-je-docker"},
		{"čćžšđ", "cczsdj"},
		{"Đorđe Čavić", "djordje-cavic"},
		{"Докер конфигурација", "doker-konfiguracija"}, // Cyrillic → Latin
		{"", "untitled"},
		{"   ", "untitled"},
		{"!!!", "untitled"},
		{"Use restart: always", "use-restart-always"},
		{"a b c", "a-b-c"},
		{"Müller café", "m-ller-caf"}, // unknown diacritics → -
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got := slugify(c.in)
			if got != c.want {
				t.Errorf("slugify(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestSlugify_TruncatesAt60(t *testing.T) {
	long := "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyz"
	got := slugify(long)
	if len([]rune(got)) > 60 {
		t.Errorf("slugify length = %d, want <= 60", len([]rune(got)))
	}
}

func TestSlugifyCyrillicFull(t *testing.T) {
	got := slugify("Шта је докер")
	want := "sta-je-doker"
	if got != want {
		t.Errorf("slugify cyrillic = %q, want %q", got, want)
	}
}

func TestTransliterateSerbian(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"Šta", "Sta"},
		{"ČAVOVIĆ", "CAVOVIC"},
		{"љубав", "ljubav"},
		{"њихов", "njihov"},
		{"џак", "dzak"},
		{"LJUBAV", "LJUBAV"}, // uppercase Latin LJ stays LJ (not recognized as a digraph)
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got := transliterateSerbian(c.in)
			if got != c.want {
				t.Errorf("transliterateSerbian(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestTruncate(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"hello", 10, "hello"},
		{"hello", 5, "hello"},
		{"hello world", 8, "hello..."},
		{"Šta je", 10, "Šta je"},
		{"Šta je Docker", 7, "Šta ..."},
		{"aaa", 2, "aa"},
		{"", 10, ""},
		{"Šta", 3, "Šta"}, // 3 rune = 6 bajtova, rune-count = 3
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got := truncate(c.in, c.n)
			if got != c.want {
				t.Errorf("truncate(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
			}
		})
	}
}

func TestFirstLine(t *testing.T) {
	cases := []struct {
		in      string
		maxLen  int
		want    string
	}{
		{"Hello world", 80, "Hello world"},
		{"First line\nSecond line", 80, "First line"},
		{"  trimmed  ", 80, "trimmed"},
		{"", 80, "Untitled"},
		{"   \n   ", 80, "Untitled"},
		{"Šta je Docker konfiguracija", 10, "Šta je Doc..."},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got := firstLine(c.in, c.maxLen)
			if got != c.want {
				t.Errorf("firstLine(%q, %d) = %q, want %q", c.in, c.maxLen, got, c.want)
			}
		})
	}
}

func TestYAMLQuote(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", `""`},
		{"prosto", "prosto"},
		{"Docker tip", "Docker tip"},
		{"Greška: NFS timeout", `"Greška: NFS timeout"`},
		{`with "quote"`, `"with \"quote\""`},
		{"multi\nline", `"multi\nline"`},
		{" leading space", `" leading space"`},
		{"trailing ", `"trailing "`},
		{"-dash", `"-dash"`},
		{"true", `"true"`},
		{"false", `"false"`},
		{"null", `"null"`},
		{"42", `"42"`},
		{"3.14", `"3.14"`},
		{"#hash", `"#hash"`},
		{"key: value", `"key: value"`},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got := yamlQuote(c.in)
			if got != c.want {
				t.Errorf("yamlQuote(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestParseFTSQuery(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"docker", "docker"},
		{"docker nginx", "docker nginx"},
		{"docker*", `"docker*"`},
		{"docker (nginx)", `"docker (nginx)"`},
		{`has "quote"`, `"has ""quote"""`},
		{"a:b", `"a:b"`},
		{"docker-nginx", `"docker-nginx"`},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got := parseFTSQuery(c.in)
			if got != c.want {
				t.Errorf("parseFTSQuery(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestParseEnvLine(t *testing.T) {
	cases := []struct {
		line    string
		key     string
		val     string
		ok      bool
	}{
		{"KEY=value", "KEY", "value", true},
		{`KEY="value"`, "KEY", "value", true},
		{"KEY='value'", "KEY", "value", true},
		{"export KEY=value", "KEY", "value", true},
		{"  KEY = value  ", "KEY", "value", true},
		{"# comment", "", "", false},
		{"", "", "", false},
		{"   ", "", "", false},
		{"NO_EQUALS", "", "", false},
		{`=onlyval`, "", "", false},
	}
	for _, c := range cases {
		t.Run(c.line, func(t *testing.T) {
			key, val, ok := parseEnvLine(c.line)
			if ok != c.ok || key != c.key || val != c.val {
				t.Errorf("parseEnvLine(%q) = (%q, %q, %v), want (%q, %q, %v)",
					c.line, key, val, ok, c.key, c.val, c.ok)
			}
		})
	}
}

func TestLoadEnv_DoesNotOverrideExisting(t *testing.T) {
	t.Setenv("KB_TEST_KEY", "from-os")

	// Write temp .env
	tmp := t.TempDir() + "/.env"
	err := os.WriteFile(tmp, []byte("KB_TEST_KEY=from-file\nOTHER_KEY=other-val\n"), 0644)
	if err != nil {
		t.Fatalf("write tmp: %v", err)
	}

	loadEnv(tmp)

	if got := os.Getenv("KB_TEST_KEY"); got != "from-os" {
		t.Errorf("existing env overridden: got %q, want %q", got, "from-os")
	}
	if got := os.Getenv("OTHER_KEY"); got != "other-val" {
		t.Errorf("new env not set: got %q, want %q", got, "other-val")
	}
}

func TestLoadEnv_QuotedValue(t *testing.T) {
	tmp := t.TempDir() + "/.env"
	err := os.WriteFile(tmp, []byte(`KB_Q='quoted value'`+"\n"), 0644)
	if err != nil {
		t.Fatalf("write tmp: %v", err)
	}
	loadEnv(tmp)
	if got := os.Getenv("KB_Q"); got != "quoted value" {
		t.Errorf("quoted value: got %q, want %q", got, "quoted value")
	}
}
