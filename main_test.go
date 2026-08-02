package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLegacyCLIInvocationGolden(t *testing.T) {
	type goldenCase struct {
		Name    string   `json:"name"`
		Argv    []string `json:"argv"`
		Command string   `json:"command"`
		Corpus  string   `json:"corpus"`
		Args    []string `json:"args"`
	}
	data, err := os.ReadFile(filepath.Join("testdata", "legacy_cli_golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []goldenCase
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			got, err := parseInvocation(tc.Argv)
			if err != nil {
				t.Fatalf("parseInvocation(%q): %v", tc.Argv, err)
			}
			if got.Command != tc.Command || got.Profile.Name != tc.Corpus || !reflect.DeepEqual(got.Args, tc.Args) {
				t.Fatalf("parseInvocation(%q) = command=%q corpus=%q args=%q, want command=%q corpus=%q args=%q",
					tc.Argv, got.Command, got.Profile.Name, got.Args, tc.Command, tc.Corpus, tc.Args)
			}
		})
	}
}

func TestCorpusAwareCLIParsing(t *testing.T) {
	for _, tc := range []struct {
		argv []string
		args []string
	}{
		{[]string{"add", "--corpus", "ai", "note", "body", "title", "tags"}, []string{"note", "body", "title", "tags"}},
		{[]string{"search", "--corpus=ai", "transformers", "10"}, []string{"transformers", "10"}},
		{[]string{"list", "--corpus", "ai", "20"}, []string{"20"}},
		{[]string{"pending", "--corpus", "ai"}, []string{}},
	} {
		got, err := parseInvocation(tc.argv)
		if err != nil {
			t.Fatalf("parseInvocation(%q): %v", tc.argv, err)
		}
		if got.Profile.Name != "ai" || !reflect.DeepEqual(got.Args, tc.args) {
			t.Errorf("parseInvocation(%q) = corpus=%q args=%q, want ai %q", tc.argv, got.Profile.Name, got.Args, tc.args)
		}
	}
}

func TestAskScopeCLIParsing(t *testing.T) {
	for _, tc := range []struct {
		argv  []string
		scope string
		args  []string
	}{
		{[]string{"ask", "--scope", "homelab", "question"}, "homelab", []string{"question"}},
		{[]string{"ask", "--scope=ai", "question", "detail"}, "ai", []string{"question", "detail"}},
		{[]string{"ask", "--scope", "both", "question"}, "both", []string{"question"}},
		{[]string{"ask", "--scope", "auto", "question"}, "auto", []string{"question"}},
	} {
		got, err := parseInvocation(tc.argv)
		if err != nil {
			t.Fatalf("parseInvocation(%q): %v", tc.argv, err)
		}
		if got.Scope != tc.scope || got.Profile.Name != "homelab" || !reflect.DeepEqual(got.Args, tc.args) {
			t.Errorf("parseInvocation(%q) = scope=%q corpus=%q args=%q, want %q homelab %q", tc.argv, got.Scope, got.Profile.Name, got.Args, tc.scope, tc.args)
		}
	}
}

func TestCorpusCLIRejectsUnsafeTargetsAndLateFlags(t *testing.T) {
	for _, argv := range [][]string{
		{"add", "--corpus", "both", "note", "body"},
		{"search", "--corpus", "/tmp/other.db", "query"},
		{"add", "note", "body", "--corpus", "ai"},
		{"ask", "--corpus", "ai", "question"},
		{"list", "--corpus", "ai", "--corpus", "homelab", "1"},
		{"add", "--corpus=ai", "--corpus=homelab", "note", "body"},
		{"ask", "--scope", "unknown", "question"},
		{"ask", "--scope", "ai", "--scope", "homelab", "question"},
		{"ask", "question", "--scope", "ai"},
		{"search", "--scope", "ai", "question"},
	} {
		if _, err := parseInvocation(argv); err == nil {
			t.Errorf("parseInvocation(%q) unexpectedly succeeded", argv)
		}
	}
}

func TestSearchViaAPIV2Contract(t *testing.T) {
	var requestBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"query":"q","requested_scope":"both","selected_scope":"both","routing_mode":"explicit","routing_reason":"explicit_scope","needs_clarification":false,"router_version":null,"degraded_corpora":[],"total_count":2,"corpora":{"homelab":{"searched":true,"available":true,"count":1,"results":[{"corpus":"homelab","entry_id":1,"ref":"homelab:1","title":"H","content":"HC","tags":"h","public_source_url":null,"link":"kb://homelab/1","distance":0.1,"relevance":0.9,"final_score":0.8}]},"ai":{"searched":true,"available":true,"count":1,"results":[{"corpus":"ai","entry_id":1,"ref":"ai:1","title":"A","content":"AC","tags":"a","public_source_url":null,"link":"kb://ai/1","distance":0.1,"relevance":0.9,"final_score":0.9}]}}}`)
	}))
	defer server.Close()

	oldURL := kbSearchAPIV2
	kbSearchAPIV2 = server.URL
	defer func() { kbSearchAPIV2 = oldURL }()
	t.Setenv("KB_V2_TOKEN_KB_CLI_LOCAL", "test-token")
	response, err := searchViaAPIV2(context.Background(), "q", "both", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if requestBody["scope"] != "both" || requestBody["allow_degraded"] != false || requestBody["top_k"] != float64(5) {
		t.Fatalf("unexpected v2 request: %#v", requestBody)
	}
	if response.TotalCount != 2 || response.Corpora["homelab"].Results[0].Ref != "homelab:1" || response.Corpora["ai"].Results[0].Ref != "ai:1" {
		t.Fatalf("unexpected v2 response: %#v", response)
	}
	formatted := formatV2Results(response)
	if !strings.Contains(formatted, "homelab:1") || !strings.Contains(formatted, "ai:1") {
		t.Fatalf("formatted response lacks qualified refs: %s", formatted)
	}
}

func TestCorpusProfilesArePhysicallySeparated(t *testing.T) {
	homelab, err := corpusProfile("homelab")
	if err != nil {
		t.Fatal(err)
	}
	ai, err := corpusProfile("ai")
	if err != nil {
		t.Fatal(err)
	}
	if homelab.DBPath == ai.DBPath || homelab.RawRoot == ai.RawRoot || homelab.ChromaCollection == ai.ChromaCollection ||
		homelab.QuarantineDir == ai.QuarantineDir || homelab.QuarantineLog == ai.QuarantineLog ||
		homelab.WatcherLock == ai.WatcherLock || homelab.WatcherState == ai.WatcherState {
		t.Fatalf("corpus profiles share a storage, quarantine, collection, lock, or state target: homelab=%+v ai=%+v", homelab, ai)
	}
	if ai.WikiIndexPath != "" {
		t.Fatalf("AI corpus unexpectedly has a generated wiki index: %q", ai.WikiIndexPath)
	}
	if _, err := corpusProfile("both"); err == nil {
		t.Fatal("non-allowlisted corpus unexpectedly resolved")
	}
}

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
		in     string
		maxLen int
		want   string
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
		{"prosto", `"prosto"`},
		{"Docker tip", `"Docker tip"`},
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
		{"tab\tvalue", `"tab\tvalue"`},
		{"carriage\rreturn", `"carriage\rreturn"`},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got := yamlQuote(c.in)
			if got != c.want {
				t.Errorf("yamlQuote(%q) = %q, want %q", c.in, got, c.want)
			}
			var roundTrip string
			if err := json.Unmarshal([]byte(got), &roundTrip); err != nil {
				t.Fatalf("yamlQuote(%q) produced invalid JSON/YAML scalar: %v", c.in, err)
			}
			if roundTrip != c.in {
				t.Errorf("yamlQuote(%q) round trip = %q", c.in, roundTrip)
			}
		})
	}
}

func TestParseFTSQuery(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"docker", `"docker"`},
		{"docker nginx", `"docker" "nginx"`},
		{"docker*", `"docker*"`},
		{"docker (nginx)", `"docker" "(nginx)"`},
		{`has "quote"`, `"has" """quote"""`},
		{"a:b", `"a:b"`},
		{"docker-nginx", `"docker-nginx"`},
		{"192.168.1.174", `"192.168.1.174"`},
		{"/opt/kb", `"/opt/kb"`},
		{"don't", `"don't"`},
		{"docker OR nginx", `"docker" "OR" "nginx"`},
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

func TestParseFTSQueryExecutesCommonHomelabQueries(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE VIRTUAL TABLE entries_fts USING fts5(content)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO entries_fts(content) VALUES (?)`,
		"192.168.1.174 /opt/kb don't docker nginx C++"); err != nil {
		t.Fatal(err)
	}

	for _, query := range []string{"192.168.1.174", "/opt/kb", "don't", "C++"} {
		t.Run(query, func(t *testing.T) {
			var count int
			err := db.QueryRow(`SELECT count(*) FROM entries_fts WHERE entries_fts MATCH ?`,
				parseFTSQuery(query)).Scan(&count)
			if err != nil {
				t.Fatalf("query %q failed: %v", query, err)
			}
			if count != 1 {
				t.Fatalf("query %q matched %d rows, want 1", query, count)
			}
		})
	}
}

func TestWriteRawFileExclusiveCreatesPrivateUniqueFiles(t *testing.T) {
	dir := t.TempDir()
	first, err := writeRawFileExclusive(dir, "note", []byte("first"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := writeRawFileExclusive(dir, "note", []byte("second"))
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("exclusive writes reused path %q", first)
	}
	for _, path := range []string{first, second} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0600 {
			t.Errorf("%s mode = %o, want 600", filepath.Base(path), got)
		}
	}
}

func TestStreamCompletionRequiresDone(t *testing.T) {
	complete := "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"
	if err := streamCompletion(context.Background(), strings.NewReader(complete), time.Now()); err != nil {
		t.Fatalf("complete stream failed: %v", err)
	}

	incomplete := "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"
	if err := streamCompletion(context.Background(), strings.NewReader(incomplete), time.Now()); err == nil {
		t.Fatal("incomplete stream returned success")
	}

	malformed := "data: {not-json}\n\ndata: [DONE]\n\n"
	if err := streamCompletion(context.Background(), strings.NewReader(malformed), time.Now()); err == nil {
		t.Fatal("malformed stream returned success")
	}
}

func TestParseEnvLine(t *testing.T) {
	cases := []struct {
		line string
		key  string
		val  string
		ok   bool
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
