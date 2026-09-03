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
	"sync"
	"syscall"
	"testing"
	"time"
)

// A bare `kb ask` must reach both corpora. "homelab" would hide the AI corpus
// again; "auto" is rejected by the router with 409 until it is calibrated.
func TestDefaultAskScopeIsBoth(t *testing.T) {
	if defaultAskScope != "both" {
		t.Fatalf("defaultAskScope = %q", defaultAskScope)
	}
}

// The v1 fallback was removed along with its endpoint variable, so this can no
// longer point a separate server at v1 and count its calls. One server serving
// the whole base keeps the guarantee: whatever path a reintroduced fallback
// chose, it would arrive here and be recorded.
func TestBareAskDoesNotFallBackToV1WhenV2Fails(t *testing.T) {
	oldV2 := kbSearchAPIV2
	defer func() { kbSearchAPIV2 = oldV2 }()

	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	t.Setenv("KB_SEARCH_API_URL", srv.URL)
	applyKBSearchBase()
	t.Setenv("KB_V2_TOKEN_KB_CLI_LOCAL", "test-token")

	profile, err := corpusProfile("homelab")
	if err != nil {
		t.Fatal(err)
	}
	err = runAsk(context.Background(), "query", "", "", "", profile, time.Now())
	if err == nil {
		t.Fatal("bare ask unexpectedly succeeded after v2 failure")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(paths) == 0 {
		t.Fatal("no request reached the server; the test would pass vacuously")
	}
	for _, p := range paths {
		if p != "/v2/kb/search" {
			t.Fatalf("non-v2 path requested after v2 failure: %q (all: %v)", p, paths)
		}
	}
}

func TestKBSearchBaseOverride(t *testing.T) {
	oldV2 := kbSearchAPIV2
	defer func() { kbSearchAPIV2 = oldV2 }()

	// Unset must leave the compiled-in default alone.
	kbSearchAPIV2 = defaultKBSearchBase + "/v2/kb/search"
	t.Setenv("KB_SEARCH_API_URL", "")
	applyKBSearchBase()
	if kbSearchAPIV2 != defaultKBSearchBase+"/v2/kb/search" {
		t.Fatalf("empty override changed endpoint: %q", kbSearchAPIV2)
	}

	// A trailing slash must not produce a doubled separator.
	t.Setenv("KB_SEARCH_API_URL", "http://127.0.0.1:9999/")
	applyKBSearchBase()
	if kbSearchAPIV2 != "http://127.0.0.1:9999/v2/kb/search" {
		t.Fatalf("v2 endpoint = %q", kbSearchAPIV2)
	}
}

// The override is applied from main after loadEnv, never at package init —
// otherwise a value living only in the env file would be read too late and
// silently ignored, leaving the default in place.
func TestKBSearchBaseIsAppliedAfterEnvFileLoad(t *testing.T) {
	oldV2 := kbSearchAPIV2
	defer func() { kbSearchAPIV2 = oldV2 }()

	dir := t.TempDir()
	envFile := filepath.Join(dir, "kb.env")
	if err := os.WriteFile(envFile, []byte("KB_SEARCH_API_URL=http://127.0.0.1:8123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KB_SEARCH_API_URL", "")
	os.Unsetenv("KB_SEARCH_API_URL")

	loadEnv(envFile)
	applyKBSearchBase()
	if kbSearchAPIV2 != "http://127.0.0.1:8123/v2/kb/search" {
		t.Fatalf("env-file value not picked up: %q", kbSearchAPIV2)
	}
}

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

func TestAskAlternateCLIParsing(t *testing.T) {
	for _, tc := range []struct {
		argv             []string
		scope, alt, lang string
	}{
		{[]string{"ask", "--alt", "other question", "--alt-lang", "en", "question"}, "", "other question", "en"},
		{[]string{"ask", "--scope", "both", "--alt", "drugo pitanje", "--alt-lang", "sr", "question"}, "both", "drugo pitanje", "sr"},
		{[]string{"ask", "--alt-lang=sr", "--alt=drugo pitanje", "--scope=homelab", "question"}, "homelab", "drugo pitanje", "sr"},
	} {
		got, err := parseInvocation(tc.argv)
		if err != nil {
			t.Fatalf("parseInvocation(%q): %v", tc.argv, err)
		}
		if got.Scope != tc.scope || got.Alt != tc.alt || got.AltLang != tc.lang || !reflect.DeepEqual(got.Args, []string{"question"}) {
			t.Errorf("parseInvocation(%q) = scope=%q alt=%q lang=%q args=%q", tc.argv, got.Scope, got.Alt, got.AltLang, got.Args)
		}
	}
}

func TestAskAlternateCLIRejectsMalformedFlags(t *testing.T) {
	for _, argv := range [][]string{
		{"ask", "--alt", "other", "question"},
		{"ask", "--alt-lang", "en", "question"},
		{"ask", "--alt", "one", "--alt", "two", "--alt-lang", "en", "question"},
		{"ask", "--alt", "other", "--alt-lang", "en", "--alt-lang", "sr", "question"},
		{"ask", "--alt", "other", "--alt-lang", "de", "question"},
		{"ask", "question", "--alt", "other", "--alt-lang", "en"},
		{"ask", "--unknown", "question"},
	} {
		if _, err := parseInvocation(argv); err == nil {
			t.Errorf("parseInvocation(%q) unexpectedly succeeded", argv)
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
	response, err := searchViaAPIV2(context.Background(), "q", "", "", "both", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if requestBody["scope"] != "both" || requestBody["allow_degraded"] != false || requestBody["top_k"] != float64(5) {
		t.Fatalf("unexpected v2 request: %#v", requestBody)
	}
	if _, ok := requestBody["query_alt"]; ok {
		t.Fatalf("unexpected empty query_alt in request: %#v", requestBody)
	}
	if response.TotalCount != 2 || response.Corpora["homelab"].Results[0].Ref != "homelab:1" || response.Corpora["ai"].Results[0].Ref != "ai:1" {
		t.Fatalf("unexpected v2 response: %#v", response)
	}
	formatted := formatV2Results(response)
	if !strings.Contains(formatted, "homelab:1") || !strings.Contains(formatted, "ai:1") {
		t.Fatalf("formatted response lacks qualified refs: %s", formatted)
	}
}

func TestSearchViaAPIV2CarriesAlternate(t *testing.T) {
	var requestBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&requestBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"query":"q","requested_scope":"homelab","selected_scope":"homelab","routing_mode":"explicit","routing_reason":"explicit_scope","needs_clarification":false,"router_version":null,"degraded_corpora":[],"total_count":0,"corpora":{"homelab":{"searched":true,"available":true,"count":0,"results":[]},"ai":{"searched":false,"available":false,"count":0,"results":[]}}}`)
	}))
	defer server.Close()
	oldURL := kbSearchAPIV2
	kbSearchAPIV2 = server.URL
	defer func() { kbSearchAPIV2 = oldURL }()
	t.Setenv("KB_V2_TOKEN_KB_CLI_LOCAL", "test-token")
	if _, err := searchViaAPIV2(context.Background(), "q", "upit", "sr", "homelab", time.Now()); err != nil {
		t.Fatal(err)
	}
	if requestBody["query_alt"] != "upit" || requestBody["query_alt_language"] != "sr" {
		t.Fatalf("alternate not serialized: %#v", requestBody)
	}
}

func TestBuildAskPromptRejectsPartialContextInference(t *testing.T) {
	response := v2SearchResponse{RequestedScope: "homelab", SelectedScope: "homelab"}
	prompt := buildAskPrompt(
		response,
		"### Example [homelab:339]\nThe default is example.m4a.",
		"How do I change the output template?",
		"- [Example](kb://homelab/339)",
	)

	required := []string{
		"Topic overlap alone is not an answer.",
		"Do not infer missing steps or settings",
		"a default value, an example, or plausible outside knowledge",
		"only partially relevant and lacks the requested detail",
		"must not extrapolate them into an answer",
		"QUESTION: How do I change the output template?",
		"scope: homelab, selected: homelab",
	}
	for _, fragment := range required {
		if !strings.Contains(prompt, fragment) {
			t.Errorf("prompt lacks partial-context guard %q", fragment)
		}
	}
	if !strings.Contains(prompt, "homelab:339") {
		t.Fatal("prompt lost supplied context")
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

// ownerOf returns the uid/gid a path actually carries on disk.
func ownerOf(t *testing.T, path string) (uint32, uint32) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("no syscall.Stat_t for %s", path)
	}
	return stat.Uid, stat.Gid
}

// TestAlignOwnerWithDirAsRoot is the case that matters: a root process writing
// into a corpus owned by an unprivileged user. Without the guard the file stays
// root-owned and the user's own tooling (kb-backup.sh) can no longer read it.
func TestAlignOwnerWithDirAsRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to reproduce the sudo case")
	}

	const wantUID, wantGID = 1000, 1000

	dir := t.TempDir()
	if err := os.Chown(dir, wantUID, wantGID); err != nil {
		t.Fatalf("chown dir: %v", err)
	}

	path := filepath.Join(dir, "note.md")
	if err := os.WriteFile(path, []byte("x"), 0600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if uid, _ := ownerOf(t, path); uid != 0 {
		t.Fatalf("precondition failed: file uid = %d, want 0", uid)
	}

	if err := alignOwnerWithDir(dir, path); err != nil {
		t.Fatalf("alignOwnerWithDir: %v", err)
	}

	uid, gid := ownerOf(t, path)
	if uid != wantUID || gid != wantGID {
		t.Errorf("file owner = %d:%d, want %d:%d", uid, gid, wantUID, wantGID)
	}
}

// TestAlignOwnerWithDirRootOwnedDir guards the opposite direction: a genuinely
// root-owned corpus must not be touched.
func TestAlignOwnerWithDirRootOwnedDir(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to reproduce the sudo case")
	}

	dir := t.TempDir()
	if err := os.Chown(dir, 0, 0); err != nil {
		t.Fatalf("chown dir: %v", err)
	}

	path := filepath.Join(dir, "note.md")
	if err := os.WriteFile(path, []byte("x"), 0600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	if err := alignOwnerWithDir(dir, path); err != nil {
		t.Fatalf("alignOwnerWithDir: %v", err)
	}

	if uid, gid := ownerOf(t, path); uid != 0 || gid != 0 {
		t.Errorf("file owner = %d:%d, want 0:0", uid, gid)
	}
}

// TestRecordQuarantineAlignsOwnerAsRoot covers the gap that killed the backup on
// 2026-08-11: writeRawFileExclusive aligned its owner, recordQuarantine did not,
// so a sudo-run `kb add` whose content tripped the scanner left a root-owned
// 0600 .orig inside a turok-owned corpus for the turok cron rsync to choke on.
func TestRecordQuarantineAlignsOwnerAsRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to reproduce the sudo case")
	}

	const wantUID, wantGID = 1000, 1000

	corpusRoot := t.TempDir()
	if err := os.Chown(corpusRoot, wantUID, wantGID); err != nil {
		t.Fatalf("chown corpus root: %v", err)
	}

	profile := CorpusProfile{
		QuarantineDir: filepath.Join(corpusRoot, "quarantine"),
		QuarantineLog: filepath.Join(corpusRoot, "quarantine.log"),
	}
	hits := []scanHit{{Pattern: "secret_assignment", Action: "redact", Value: "hunter2"}}

	backupPath := recordQuarantine(profile, "token = hunter2", "token = [REDACTED]", "cli", "note", hits)
	if backupPath == "" {
		t.Fatal("no quarantine backup written")
	}

	for _, path := range []string{profile.QuarantineDir, backupPath, profile.QuarantineLog} {
		uid, gid := ownerOf(t, path)
		if uid != wantUID || gid != wantGID {
			t.Errorf("%s owner = %d:%d, want %d:%d", path, uid, gid, wantUID, wantGID)
		}
	}
}

// TestAlignOwnerWithDirNonRoot documents that the normal path stays a no-op —
// an unprivileged `kb add` must never attempt a chown it cannot perform.
func TestAlignOwnerWithDirNonRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("this case is about the unprivileged path")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "note.md")
	if err := os.WriteFile(path, []byte("x"), 0600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	wantUID, wantGID := ownerOf(t, path)
	if err := alignOwnerWithDir(dir, path); err != nil {
		t.Fatalf("alignOwnerWithDir: %v", err)
	}
	if uid, gid := ownerOf(t, path); uid != wantUID || gid != wantGID {
		t.Errorf("file owner changed to %d:%d, want %d:%d", uid, gid, wantUID, wantGID)
	}
}

// TestCheckAddArgShape locks in the guard for the corruption that produced
// entries 606-608 on 2026-08-11: `kb add` takes positional arguments, a
// flag-style invocation shifted every value one slot to the left, and nothing
// downstream noticed a title that was literally "--title".
func TestCheckAddArgShape(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name:    "flag in title slot is the 606-608 case",
			args:    []string{"note", "body text", "--title", "Laguna XS Forrix local evaluation"},
			wantErr: `"--title" is an option, not a value, in the title slot`,
		},
		{
			name:    "flag in tags slot",
			args:    []string{"note", "body text", "Real Title", "--tags"},
			wantErr: `"--tags" is an option, not a value, in the tags slot`,
		},
		{
			name:    "flag in content slot",
			args:    []string{"note", "--title", "Real Title", "a,b"},
			wantErr: `"--title" is an option, not a value, in the content slot`,
		},
		{
			name:    "flag-style call also overflows the slot count",
			args:    []string{"note", "body", "--title", "Real Title", "--tags", "a,b"},
			wantErr: "too many arguments (6)",
		},
		{name: "valid note", args: []string{"note", "body", "Title", "a,b"}},
		{name: "valid stdin note", args: []string{"note", "-", "Title", "a,b"}},
		{name: "valid url", args: []string{"url", "https://example.com", "Title", "a,b"}},
		{name: "type only", args: []string{"note"}},

		// False positives would be worse than the bug: these are all real note
		// content that starts with dashes and must stay storable.
		{name: "yaml frontmatter content", args: []string{"note", "---\ntype: note\n---\n"}},
		{name: "diff header content", args: []string{"note", "--- a/main.go\n+++ b/main.go"}},
		{name: "bare double dash", args: []string{"note", "--", "Title"}},
		{name: "em dash title", args: []string{"note", "body", "KB health — orphan 584"}},
		{name: "flag-looking text with a space", args: []string{"note", "body", "--title is positional"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkAddArgShape(tc.args)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("checkAddArgShape(%q) = %v, want nil", tc.args, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("checkAddArgShape(%q) = nil, want error containing %q", tc.args, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("checkAddArgShape(%q) = %q, want it to contain %q", tc.args, err, tc.wantErr)
			}
		})
	}
}

// TestCompileArgv guards the --corpus routing: compile.py runs configure_corpus
// on whatever it is passed, so a dropped flag silently retires from homelab.
func TestCompileArgv(t *testing.T) {
	homelab, err := corpusProfile("homelab")
	if err != nil {
		t.Fatalf("homelab profile: %v", err)
	}
	ai, err := corpusProfile("ai")
	if err != nil {
		t.Fatalf("ai profile: %v", err)
	}

	want := []string{"/opt/kb/venv-embed/bin/python", "/opt/kb/compile.py", "--retire", "584"}
	if got := homelab.compileArgv("--retire", "584"); !reflect.DeepEqual(got, want) {
		t.Errorf("homelab compileArgv = %q, want %q", got, want)
	}

	want = []string{"/opt/kb/venv-embed/bin/python", "/opt/kb/compile.py", "--corpus", "ai", "--retire", "584"}
	if got := ai.compileArgv("--retire", "584"); !reflect.DeepEqual(got, want) {
		t.Errorf("ai compileArgv = %q, want %q", got, want)
	}

	if got, want := homelab.compileCommand(), "/opt/kb/venv-embed/bin/python /opt/kb/compile.py"; got != want {
		t.Errorf("homelab compileCommand = %q, want %q", got, want)
	}
	if got, want := ai.compileCommand(), "/opt/kb/venv-embed/bin/python /opt/kb/compile.py --corpus ai"; got != want {
		t.Errorf("ai compileCommand = %q, want %q", got, want)
	}
}

// TestRetireIsARoutedCommand covers the wiring: an unknown command must not
// reach the dispatcher, and `retire` must accept --corpus like its siblings.
func TestRetireIsARoutedCommand(t *testing.T) {
	invocation, err := parseInvocation([]string{"retire", "--corpus", "ai", "584"})
	if err != nil {
		t.Fatalf("parseInvocation(retire --corpus ai 584): %v", err)
	}
	if invocation.Command != "retire" {
		t.Errorf("Command = %q, want %q", invocation.Command, "retire")
	}
	if invocation.Profile.Name != "ai" {
		t.Errorf("Profile.Name = %q, want %q", invocation.Profile.Name, "ai")
	}
	if want := []string{"584"}; !reflect.DeepEqual(invocation.Args, want) {
		t.Errorf("Args = %q, want %q", invocation.Args, want)
	}
}
