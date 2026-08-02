package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	_ "github.com/mattn/go-sqlite3"
)

const (
	openRouterURL = "https://openrouter.ai/api/v1/chat/completions"
	defaultModel  = "google/gemini-2.5-flash-lite"
	kbSearchAPI   = "http://192.168.1.174:8050/kb/search"
)

var (
	httpQuick     = &http.Client{Timeout: 30 * time.Second}
	httpSlow      = &http.Client{Timeout: 150 * time.Second}
	kbSearchAPIV2 = "http://192.168.1.174:8050/v2/kb/search"
)

type Result struct {
	ID      int64
	Title   string
	Content string
	Summary string
	Tags    string
	Source  string
	Date    string
	Score   float64
}

type commandInvocation struct {
	Command string
	Args    []string
	Profile CorpusProfile
	Scope   string
}

type singleCorpusFlag struct {
	value *string
	set   bool
}

type singleScopeFlag struct {
	value *string
	set   bool
}

func (f *singleScopeFlag) String() string {
	if f == nil || f.value == nil {
		return ""
	}
	return *f.value
}

func (f *singleScopeFlag) Set(value string) error {
	if f.set {
		return errors.New("--scope may be specified only once")
	}
	f.set = true
	*f.value = value
	return nil
}

func (f *singleCorpusFlag) String() string {
	if f == nil || f.value == nil {
		return defaultCorpus
	}
	return *f.value
}

func (f *singleCorpusFlag) Set(value string) error {
	if f.set {
		return errors.New("--corpus may be specified only once")
	}
	f.set = true
	*f.value = value
	return nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	invocation, err := parseInvocation(os.Args[1:])
	if err != nil {
		if err.Error() != "usage" {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
		printUsage()
		os.Exit(1)
	}
	loadEnv(invocation.Profile.EnvFile)

	switch invocation.Command {
	case "ask":
		cmdAsk(ctx, invocation.Args, invocation.Profile, invocation.Scope)
	case "add":
		cmdAdd(ctx, invocation.Args, invocation.Profile)
	case "list":
		cmdList(ctx, invocation.Args, invocation.Profile)
	case "search":
		cmdSearch(ctx, invocation.Args, invocation.Profile)
	case "pending":
		cmdPending(ctx, invocation.Profile)
	}
}

func parseInvocation(args []string) (commandInvocation, error) {
	if len(args) == 0 {
		return commandInvocation{}, errors.New("usage")
	}

	command := args[0]
	switch command {
	case "ask", "add", "list", "search", "pending":
	default:
		return commandInvocation{}, fmt.Errorf("unknown command %q", command)
	}

	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	corpusName := defaultCorpus
	scope := ""
	if command == "ask" {
		fs.Var(&singleScopeFlag{value: &scope}, "scope", "search scope (homelab, ai, both, or auto)")
	} else {
		fs.Var(&singleCorpusFlag{value: &corpusName}, "corpus", "target corpus (homelab or ai)")
	}
	commandArgs := args[1:]
	flagArgs := []string{}
	if command == "ask" && len(commandArgs) > 0 &&
		(commandArgs[0] == "--scope" || strings.HasPrefix(commandArgs[0], "--scope=")) {
		flagArgs = commandArgs
	} else if command != "ask" && len(commandArgs) > 0 &&
		(commandArgs[0] == "--corpus" || strings.HasPrefix(commandArgs[0], "--corpus=")) {
		flagArgs = commandArgs
	}
	if err := fs.Parse(flagArgs); err != nil {
		return commandInvocation{}, err
	}
	positionalArgs := commandArgs
	if len(flagArgs) > 0 {
		positionalArgs = fs.Args()
	}
	for _, arg := range positionalArgs {
		if arg == "--corpus" || strings.HasPrefix(arg, "--corpus=") {
			return commandInvocation{}, errors.New("--corpus must appear immediately after the command")
		}
		if arg == "--scope" || strings.HasPrefix(arg, "--scope=") {
			return commandInvocation{}, errors.New("--scope is valid only for ask and must appear immediately after the command")
		}
	}
	if scope != "" && scope != "homelab" && scope != "ai" && scope != "both" && scope != "auto" {
		return commandInvocation{}, fmt.Errorf("unknown scope %q (allowed: homelab, ai, both, auto)", scope)
	}

	profile, err := corpusProfile(corpusName)
	if err != nil {
		return commandInvocation{}, err
	}
	return commandInvocation{Command: command, Args: positionalArgs, Profile: profile, Scope: scope}, nil
}

func printUsage() {
	fmt.Println(`Usage: kb <command> [args...]

Commands:
  ask [--scope homelab|ai|both|auto] "question"       Semantic search + LLM synthesis
  add [--corpus homelab|ai] note "content" "title" "tags"
  add [--corpus homelab|ai] note - "title" "tags"   Add from stdin
  add [--corpus homelab|ai] url "url" "title" "tags"
  list [--corpus homelab|ai] [limit]                  List entries
  search [--corpus homelab|ai] "query" [limit]        FTS5 search
  pending [--corpus homelab|ai]                       Pending entries

Examples:
  kb ask "how does NFS work in the homelab?"
  kb add note "Docker tip" "Use restart: always" "docker,tips"
  kb add note - "My Note" "tag1" <<'EOF'
  ...long content...
  EOF
  kb add url "https://example.com" "Interesting article" "bookmarks"
  kb list 10
  kb search "docker"
  kb search --corpus ai "transformers" 10
  kb pending`)
}

// ─── ask ──────────────────────────────────────────────────────────────────────

func cmdAsk(ctx context.Context, args []string, profile CorpusProfile, invocationScope string) {
	start := time.Now()

	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: kb ask \"question\"")
		os.Exit(1)
	}

	query := strings.Join(args, " ")
	if invocationScope != "" {
		if err := cmdAskV2(ctx, query, invocationScope, profile, start); err != nil {
			os.Exit(1)
		}
		return
	}

	results, err := searchViaAPI(ctx, query, start)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%v] KB Search API error: %v\n",
			time.Since(start).Round(time.Millisecond), err)
		fmt.Fprintf(os.Stderr, "[%v] Search unavailable — cannot answer.\n",
			time.Since(start).Round(time.Millisecond))
		os.Exit(1)
	}

	if len(results) == 0 {
		fmt.Fprintf(os.Stderr, "[%v] KB Search API returned 0 results.\n",
			time.Since(start).Round(time.Millisecond))
	} else {
		fmt.Fprintf(os.Stderr, "[%v] KB Search API — %d results after rerank\n",
			time.Since(start).Round(time.Millisecond), len(results))
	}

	context := formatResults(results)
	if os.Getenv("KB_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "--- DEBUG: Context ---\n%s\n--- END DEBUG ---\n", context)
	}

	indexData, err := os.ReadFile(profile.WikiIndexPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%v] warn: wiki index missing (%v)\n",
			time.Since(start).Round(time.Millisecond), err)
	} else {
		fmt.Fprintf(os.Stderr, "[%v] Index loaded (%d bytes)\n",
			time.Since(start).Round(time.Millisecond), len(indexData))
	}

	prompt := fmt.Sprintf(`You are an assistant that answers solely based on content from a personal knowledge base.

RULES:
- Answer in detail and with specifics, using all relevant information from the context below.
- Cite source titles when possible.
- If the information is not present in the KB, explicitly say so — do not fabricate.
- If the context below is empty or says "No relevant results", respond with: "I couldn't find relevant information in the knowledge base for this query."
- Do not shorten the answer — the user wants as much detail as possible from the KB.
- Keep technical terms, project names, commands, and URLs in their original form — do not translate them.
- Results are tagged HIGH/MEDIUM/LOW relevance — prefer HIGH over LOW; treat LOW as weak evidence.

---
RELEVANT KB RESULTS (strategy: api-rerank):
%s

---
QUESTION: %s

---
Wiki index (for orientation only, not for direct answers):
%s`, context, query, string(indexData))

	if err := callOpenRouter(ctx, prompt, start); err != nil {
		fmt.Fprintf(os.Stderr, "[%v] OpenRouter did not respond: %v\n",
			time.Since(start).Round(time.Millisecond), err)
		fmt.Fprintf(os.Stderr, "[%v] Here are raw KB results:\n",
			time.Since(start).Round(time.Millisecond))
		fmt.Println(context)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "[%v] Done.\n", time.Since(start).Round(time.Millisecond))
}

// ─── add ──────────────────────────────────────────────────────────────────────

func cmdAdd(ctx context.Context, args []string, profile CorpusProfile) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: kb add <note|url> ...")
		fmt.Fprintln(os.Stderr, "  kb add note \"content\" \"title\" \"tags\"")
		fmt.Fprintln(os.Stderr, "  kb add note - \"title\" \"tags\"  (read content from stdin)")
		fmt.Fprintln(os.Stderr, "  kb add url \"url\" \"title\" \"tags\"")
		os.Exit(1)
	}

	if err := ctx.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "Aborted: %v\n", err)
		os.Exit(1)
	}

	entryType := args[0]
	if entryType != "note" && entryType != "url" {
		fmt.Fprintf(os.Stderr, "Invalid type: %s (must be 'note' or 'url')\n", entryType)
		os.Exit(1)
	}

	var content, title, tags string

	if entryType == "note" && len(args) > 1 && args[1] == "-" {
		if len(args) >= 3 {
			title = args[2]
		}
		if len(args) >= 4 {
			tags = args[3]
		}
		stdinBytes, err := io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error reading stdin: %v\n", err)
			os.Exit(1)
		}
		content = string(stdinBytes)
		if content == "" {
			fmt.Fprintln(os.Stderr, "Error: no content provided on stdin")
			os.Exit(1)
		}
	} else {
		if len(args) < 2 {
			fmt.Fprintf(os.Stderr, "Error: content is required for %s\n", entryType)
			os.Exit(1)
		}
		content = args[1]
		if len(args) >= 3 {
			title = args[2]
		}
		if len(args) >= 4 {
			tags = args[3]
		}
	}

	// Gate 1: redact secrets before content reaches SQLite/FTS or the raw file.
	origContent := content
	var scanHits []scanHit
	var rerr error
	content, title, scanHits, rerr = sanitizeWrite(profile, content, title)
	if rerr != nil {
		fmt.Fprintf(os.Stderr, "Error: secret scanner unavailable for corpus %s: %v\n", profile.Name, rerr)
		os.Exit(1)
	}

	if title == "" {
		title = firstLine(content, 80)
	}

	now := time.Now()
	ts := now.Format("2006-01-02T15:04:05")
	dateStr := now.Format("2006-01-02")
	slug := slugify(title)
	if slug == "" {
		slug = slugify(content)
	}
	if slug == "" {
		slug = "untitled"
	}

	if len(scanHits) > 0 {
		backup := recordQuarantine(profile, origContent, content, "cli", slug, scanHits)
		names := make([]string, 0, len(scanHits))
		for _, h := range scanHits {
			names = append(names, h.Pattern+"("+h.Action+")")
		}
		fmt.Fprintf(os.Stderr, "kb add: redacted %d secret(s) before write: %s\n",
			len(scanHits), strings.Join(names, ", "))
		if backup != "" {
			fmt.Fprintf(os.Stderr, "         original: %s\n", backup)
		}
	}

	subdir := "notes"
	if entryType == "url" {
		subdir = "urls"
	}

	rawSubdir := fmt.Sprintf("%s/%s", profile.RawRoot, subdir)
	if err := os.MkdirAll(rawSubdir, 0700); err != nil {
		fmt.Fprintf(os.Stderr, "Error creating directory: %v\n", err)
		os.Exit(1)
	}
	if err := os.Chmod(rawSubdir, 0700); err != nil {
		fmt.Fprintf(os.Stderr, "Error securing directory: %v\n", err)
		os.Exit(1)
	}

	frontmatter := fmt.Sprintf(`---
type: %s
title: %s
tags: %s
saved: %s
---

%s
`, entryType, yamlQuote(title), yamlQuote(tags), ts, content)
	rawPath, err := writeRawFileExclusive(rawSubdir, fmt.Sprintf("%s-%s", dateStr, slug), []byte(frontmatter))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error writing file: %v\n", err)
		os.Exit(1)
	}

	db, err := openDB(profile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening database: %v\n", err)
		if rmErr := os.Remove(rawPath); rmErr != nil {
			fmt.Fprintf(os.Stderr, "warn: could not remove orphan file %s: %v\n", rawPath, rmErr)
		}
		os.Exit(1)
	}
	defer db.Close()

	result, err := db.ExecContext(ctx,
		`INSERT INTO entries (type, content, title, tags, raw_path, source, created_at)
		 VALUES (?, ?, ?, ?, ?, 'cli', ?)`,
		entryType, content, title, tags, rawPath, ts,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error inserting into database: %v\n", err)
		if rmErr := os.Remove(rawPath); rmErr != nil {
			fmt.Fprintf(os.Stderr, "warn: could not remove orphan file %s: %v\n", rawPath, rmErr)
		}
		os.Exit(1)
	}

	id, lastIDErr := result.LastInsertId()
	if lastIDErr != nil {
		fmt.Fprintf(os.Stderr, "warn: could not fetch last insert id: %v\n", lastIDErr)
	}
	fmt.Printf("OK [id=%d] %s\n", id, rawPath)
}

// ─── list ─────────────────────────────────────────────────────────────────────

func cmdList(ctx context.Context, args []string, profile CorpusProfile) {
	limit := 20
	if len(args) >= 1 {
		if n, err := strconv.Atoi(args[0]); err == nil && n > 0 {
			limit = n
		}
	}

	db, err := openDB(profile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx,
		`SELECT id, type, title, created_at FROM entries
		 ORDER BY created_at DESC LIMIT ?`, limit,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error querying database: %v\n", err)
		os.Exit(1)
	}
	defer rows.Close()

	fmt.Printf("%-6s %-6s %-50s %s\n", "ID", "TYPE", "TITLE", "CREATED")
	fmt.Println(strings.Repeat("-", 90))
	for rows.Next() {
		var id int64
		var typ, title, created string
		if err := rows.Scan(&id, &typ, &title, &created); err != nil {
			fmt.Fprintf(os.Stderr, "warn: scan error, skipping row: %v\n", err)
			continue
		}
		fmt.Printf("%-6d %-6s %-50s %s\n", id, typ, truncate(title, 50), created)
	}
	if err := rows.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "Error iterating rows: %v\n", err)
		os.Exit(1)
	}
}

// ─── search ───────────────────────────────────────────────────────────────────

func cmdSearch(ctx context.Context, args []string, profile CorpusProfile) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: kb search \"query\" [limit]")
		os.Exit(1)
	}

	query := parseFTSQuery(args[0])
	if query == "" {
		fmt.Fprintln(os.Stderr, "Error: empty search query")
		os.Exit(1)
	}

	limit := 20
	if len(args) >= 2 {
		if n, err := strconv.Atoi(args[1]); err == nil && n > 0 {
			limit = n
		}
	}

	db, err := openDB(profile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx,
		`SELECT e.id, e.type, e.title, e.content, e.tags, e.created_at
		 FROM entries e
		 JOIN entries_fts fts ON e.id = fts.rowid
		 WHERE entries_fts MATCH ?
		 ORDER BY rank
		 LIMIT ?`, query, limit,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error searching: %v\n", err)
		fmt.Fprintf(os.Stderr, "Hint: query was escaped to: %s\n", query)
		os.Exit(1)
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var id int64
		var typ, title, content, tags, created string
		if err := rows.Scan(&id, &typ, &title, &content, &tags, &created); err != nil {
			fmt.Fprintf(os.Stderr, "warn: scan error, skipping row: %v\n", err)
			continue
		}
		fmt.Printf("=== [%d] %s | %s | %s ===\n", id, typ, title, created)
		if tags != "" {
			fmt.Printf("Tags: %s\n", tags)
		}
		fmt.Println(truncate(content, 500))
		fmt.Println()
		count++
	}
	if err := rows.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "Error iterating rows: %v\n", err)
		os.Exit(1)
	}

	if count == 0 {
		fmt.Println("No matches found.")
	}
}

// ─── pending ──────────────────────────────────────────────────────────────────

func cmdPending(ctx context.Context, profile CorpusProfile) {
	db, err := openDB(profile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx,
		`SELECT id, type, title, raw_path FROM entries
		 WHERE embedded_at IS NULL ORDER BY created_at`,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error querying database: %v\n", err)
		os.Exit(1)
	}
	defer rows.Close()

	fmt.Printf("%-6s %-6s %-40s %s\n", "ID", "TYPE", "TITLE", "RAW_PATH")
	fmt.Println(strings.Repeat("-", 100))
	count := 0
	for rows.Next() {
		var id int64
		var typ, title, rawPath string
		if err := rows.Scan(&id, &typ, &title, &rawPath); err != nil {
			fmt.Fprintf(os.Stderr, "warn: scan error, skipping row: %v\n", err)
			continue
		}
		fmt.Printf("%-6d %-6s %-40s %s\n", id, typ, truncate(title, 40), rawPath)
		count++
	}
	if err := rows.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "Error iterating rows: %v\n", err)
		os.Exit(1)
	}

	if count == 0 {
		fmt.Println("All entries embedded.")
	} else {
		fmt.Printf("\n%d entries pending embedding.\nRun: %s\n", count, profile.compileCommand())
	}
}

// ─── helpers ──────────────────────────────────────────────────────────────────

// slugify transliterates Serbian Latin and Cyrillic characters to ASCII and produces a URL-safe slug.
func slugify(s string) string {
	s = transliterateSerbian(strings.ToLower(s))
	var result strings.Builder
	lastDash := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			result.WriteRune(r)
			lastDash = false
		} else if unicode.IsLetter(r) || unicode.IsDigit(r) {
			// Unrecognized unicode letter — replace with dash to preserve word boundary
			if !lastDash {
				result.WriteRune('-')
				lastDash = true
			}
		} else if !lastDash {
			result.WriteRune('-')
			lastDash = true
		}
	}
	out := strings.Trim(result.String(), "-")
	if len(out) > 60 {
		runes := []rune(out)
		if len(runes) > 60 {
			out = string(runes[:60])
		}
	}
	if out == "" {
		out = "untitled"
	}
	return out
}

// transliterateSerbian maps Serbian Latin and Cyrillic characters to their ASCII equivalents.
func transliterateSerbian(s string) string {
	var sb strings.Builder
	sb.Grow(len(s))
	for _, r := range s {
		if rep, ok := serbianMap[r]; ok {
			sb.WriteString(rep)
			continue
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

var serbianMap = map[rune]string{
	// Latin diacritics (uppercase → uppercase ASCII, lowercase → lowercase ASCII)
	'š': "s", 'Š': "S",
	'č': "c", 'Č': "C",
	'ć': "c", 'Ć': "C",
	'ž': "z", 'Ž': "Z",
	'đ': "dj", 'Đ': "Dj",
	// Cyrillic (Serbian)
	'а': "a", 'А': "a", 'б': "b", 'Б': "b", 'в': "v", 'В': "v",
	'г': "g", 'Г': "g", 'д': "d", 'Д': "d", 'ђ': "dj", 'Ђ': "dj",
	'е': "e", 'Е': "e", 'ж': "z", 'Ж': "z", 'з': "z", 'З': "z",
	'и': "i", 'И': "i", 'ј': "j", 'Ј': "j", 'к': "k", 'К': "k",
	'л': "l", 'Л': "l", 'љ': "lj", 'Љ': "lj", 'м': "m", 'М': "m",
	'н': "n", 'Н': "n", 'њ': "nj", 'Њ': "nj", 'о': "o", 'О': "o",
	'п': "p", 'П': "p", 'р': "r", 'Р': "r", 'с': "s", 'С': "s",
	'т': "t", 'Т': "t", 'ћ': "c", 'Ћ': "c", 'у': "u", 'У': "u",
	'ф': "f", 'Ф': "f", 'х': "h", 'Х': "h", 'ц': "c", 'Ц': "c",
	'ч': "c", 'Ч': "c", 'џ': "dz", 'Џ': "dz", 'ш': "s", 'Ш': "s",
}

func firstLine(s string, maxLen int) string {
	lines := strings.SplitN(strings.TrimSpace(s), "\n", 2)
	out := strings.TrimSpace(lines[0])
	if utf8.RuneCountInString(out) > maxLen {
		runes := []rune(out)
		out = string(runes[:maxLen]) + "..."
	}
	if out == "" {
		out = "Untitled"
	}
	return out
}

// truncate is a rune-safe string shortener (replaces the former truncateASCII).
func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	runes := []rune(s)
	if n <= 3 {
		return string(runes[:n])
	}
	return string(runes[:n-3]) + "..."
}

// yamlQuote returns a JSON string literal, which is also a valid YAML scalar.
// Always quoting avoids YAML's implicit booleans/numbers and control-character edge cases.
func yamlQuote(s string) string {
	quoted, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(quoted)
}

// parseFTSQuery treats every whitespace-delimited term as a literal FTS5 phrase.
// This preserves implicit AND semantics while making IPs, paths, apostrophes, and
// operator words safe. Advanced FTS syntax is intentionally not accepted here.
func parseFTSQuery(s string) string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return ""
	}
	quoted := make([]string, 0, len(fields))
	for _, field := range fields {
		escaped := strings.ReplaceAll(field, `"`, `""`)
		quoted = append(quoted, `"`+escaped+`"`)
	}
	return strings.Join(quoted, " ")
}

// writeRawFileExclusive creates a private raw file without ever overwriting an
// existing entry. O_EXCL is the collision guarantee; the nanosecond suffix only
// keeps the common path to a single attempt.
func writeRawFileExclusive(dir, prefix string, data []byte) (string, error) {
	for attempt := 0; attempt < 100; attempt++ {
		suffix := time.Now().UnixNano()
		path := fmt.Sprintf("%s/%s-%d.md", dir, prefix, suffix)
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}

		_, writeErr := file.Write(data)
		closeErr := file.Close()
		if writeErr != nil || closeErr != nil {
			if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				fmt.Fprintf(os.Stderr, "warn: could not remove incomplete file %s: %v\n", path, removeErr)
			}
			if writeErr != nil {
				return "", writeErr
			}
			return "", closeErr
		}
		return path, nil
	}
	return "", fmt.Errorf("could not allocate unique raw filename after 100 attempts")
}

// openDB opens SQLite with WAL and busy_timeout (prevents "database is locked" errors).
func openDB(profile CorpusProfile) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", profile.sqliteDSN())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// ─── OpenRouter ───────────────────────────────────────────────────────────────

func callOpenRouter(ctx context.Context, prompt string, start time.Time) error {
	apiKey := os.Getenv("OPENROUTER_API_KEY")
	if apiKey == "" {
		return fmt.Errorf("OPENROUTER_API_KEY not set")
	}

	modelToUse := os.Getenv("OPENROUTER_MODEL")
	if modelToUse == "" {
		modelToUse = defaultModel
	}

	fmt.Fprintf(os.Stderr, "[%v] Calling OpenRouter (%s)...\n",
		time.Since(start).Round(time.Millisecond), modelToUse)

	body, err := json.Marshal(map[string]any{
		"model": modelToUse,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
		"stream": true,
	})
	if err != nil {
		return fmt.Errorf("json marshal: %v", err)
	}

	for attempt := 0; attempt < 2; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		req, err := http.NewRequestWithContext(ctx, "POST", openRouterURL, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("Content-Type", "application/json")

		resp, err := httpSlow.Do(req)
		if err != nil {
			return err
		}

		if resp.StatusCode == http.StatusTooManyRequests && attempt == 0 {
			retryAfter := 2 * time.Second
			if ra := resp.Header.Get("Retry-After"); ra != "" {
				if sec, perr := strconv.Atoi(ra); perr == nil && sec > 0 {
					retryAfter = time.Duration(sec) * time.Second
				}
			}
			resp.Body.Close()
			fmt.Fprintf(os.Stderr, "[%v] 429 rate limited, retrying after %v...\n",
				time.Since(start).Round(time.Millisecond), retryAfter)
			select {
			case <-time.After(retryAfter):
			case <-ctx.Done():
				return ctx.Err()
			}
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("API error %d: %s", resp.StatusCode, string(b))
		}

		fmt.Fprintf(os.Stderr, "[%v] Stream started...\n", time.Since(start).Round(time.Millisecond))
		return streamCompletion(ctx, resp.Body, start)
	}
	return fmt.Errorf("OpenRouter rate limited after retry")
}

// streamCompletion prints the SSE stream from OpenRouter. Chunk is declared inside
// the loop to avoid the reuse bug (previous content value would repeat on empty delta).
func streamCompletion(ctx context.Context, body io.Reader, start time.Time) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	sawDone := false

	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			sawDone = true
			break
		}
		var chunk struct {
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return fmt.Errorf("invalid SSE data: %w", err)
		}
		if chunk.Error != nil {
			return fmt.Errorf("stream error: %s", chunk.Error.Message)
		}
		if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" {
			fmt.Print(chunk.Choices[0].Delta.Content)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if !sawDone {
		return fmt.Errorf("OpenRouter stream ended before [DONE]")
	}
	fmt.Println()
	return nil
}

// ─── KB Search API client ─────────────────────────────────────────────────────

// searchViaAPI calls the KB Search API with retry. 4xx errors are non-retryable.
func searchViaAPI(ctx context.Context, query string, start time.Time) ([]Result, error) {
	fmt.Fprintf(os.Stderr, "[%v] Calling KB Search API...\n",
		time.Since(start).Round(time.Millisecond))

	body, err := json.Marshal(map[string]string{
		"query":  query,
		"format": "full",
	})
	if err != nil {
		return nil, fmt.Errorf("json marshal: %v", err)
	}

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if attempt > 0 {
			backoff := time.Duration(500*(1<<(attempt-1))) * time.Millisecond
			fmt.Fprintf(os.Stderr, "[%v] Retry %d/3 (backoff %v)...\n",
				time.Since(start).Round(time.Millisecond), attempt+1, backoff)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}

		req, err := http.NewRequestWithContext(ctx, "POST", kbSearchAPI, bytes.NewReader(body))
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := httpQuick.Do(req)
		if err != nil {
			lastErr = err
			continue
		}

		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return nil, fmt.Errorf("API error %d (non-retryable): %s", resp.StatusCode, string(b))
		}
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			lastErr = fmt.Errorf("API error %d: %s", resp.StatusCode, string(b))
			continue
		}

		var apiResp struct {
			Results []struct {
				ID         int     `json:"id"`
				Title      string  `json:"title"`
				Content    string  `json:"content"`
				Summary    string  `json:"summary"`
				Tags       string  `json:"tags"`
				Source     string  `json:"source"`
				Date       string  `json:"date"`
				Distance   float64 `json:"distance"`
				Relevance  float64 `json:"relevance"`
				FinalScore float64 `json:"final_score"`
			} `json:"results"`
			Query string `json:"query"`
			Count int    `json:"count"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
			resp.Body.Close()
			lastErr = err
			continue
		}
		resp.Body.Close()

		var results []Result
		for _, r := range apiResp.Results {
			results = append(results, Result{
				ID:      int64(r.ID),
				Title:   r.Title,
				Content: r.Content,
				Summary: r.Summary,
				Tags:    r.Tags,
				Source:  r.Source,
				Date:    r.Date,
				Score:   r.FinalScore,
			})
		}
		fmt.Fprintf(os.Stderr, "[%v] KB Search API returned %d results\n",
			time.Since(start).Round(time.Millisecond), len(results))
		return results, nil
	}
	return nil, fmt.Errorf("KB Search API unavailable after 3 attempts: %v", lastErr)
}

// ─── formatting ───────────────────────────────────────────────────────────────

func formatResults(results []Result) string {
	if len(results) == 0 {
		return "(No relevant KB results found for this query.)"
	}
	var sb strings.Builder
	for _, r := range results {
		title := r.Title
		if title == "" {
			title = "Untitled"
		}
		sb.WriteString(fmt.Sprintf("### %s [%s] (%s) — %s\n", title, r.Source, r.Date, relevanceLabel(r.Score)))
		if r.Summary != "" {
			sb.WriteString(fmt.Sprintf("Summary: %s\n", r.Summary))
		}
		if r.Tags != "" {
			sb.WriteString(fmt.Sprintf("Tags: %s\n", r.Tags))
		}
		sb.WriteString(truncate(r.Content, 3000))
		sb.WriteString("\n\n---\n\n")
	}
	return sb.String()
}

// relevanceLabel maps a final_score (relevance × decay) to a qualitative band
// so the LLM can weigh evidence without knowing the numeric scale.
func relevanceLabel(score float64) string {
	switch {
	case score >= 0.60:
		return "HIGH relevance"
	case score >= 0.30:
		return "MEDIUM relevance"
	default:
		return "LOW relevance"
	}
}

// ─── env loading ──────────────────────────────────────────────────────────────

func loadEnv(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		key, val, ok := parseEnvLine(line)
		if !ok {
			continue
		}
		// Existing env takes precedence — do not override.
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		os.Setenv(key, val)
	}
}

// parseEnvLine parses a single .env file line. Returns (key, val, ok).
// Supports: KEY=value, KEY="value", KEY='value', export KEY=value.
// Skips: empty lines, comments (#), lines without =.
func parseEnvLine(line string) (key, val string, ok bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	if strings.HasPrefix(line, "export ") {
		line = strings.TrimPrefix(line, "export ")
		line = strings.TrimSpace(line)
	}
	parts := strings.SplitN(line, "=", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	key = strings.TrimSpace(parts[0])
	val = strings.TrimSpace(parts[1])
	if (len(val) >= 2) &&
		((val[0] == '"' && val[len(val)-1] == '"') ||
			(val[0] == '\'' && val[len(val)-1] == '\'')) {
		val = val[1 : len(val)-1]
	}
	if key == "" {
		return "", "", false
	}
	return key, val, true
}
