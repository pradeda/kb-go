package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
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
	dbPath        = "/opt/kb/kb.db"
	sqliteDSN     = dbPath + "?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on"
	rawDir        = "/opt/kb/raw"
	wikiIndex     = "/opt/kb/wiki/index.md"
	openRouterURL = "https://openrouter.ai/api/v1/chat/completions"
	defaultModel  = "google/gemini-2.5-flash-lite"
	kbSearchAPI   = "http://192.168.1.174:8050/kb/search"
)

var (
	httpQuick = &http.Client{Timeout: 30 * time.Second}
	httpSlow  = &http.Client{Timeout: 60 * time.Second}
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

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	loadEnv("/opt/kb/.env")

	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "ask":
		cmdAsk(ctx)
	case "add":
		cmdAdd(ctx)
	case "list":
		cmdList(ctx)
	case "search":
		cmdSearch(ctx)
	case "pending":
		cmdPending(ctx)
	default:
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`Usage: kb <command> [args...]

Commands:
  ask "question"                 Semantic search + LLM synthesis
  add note "content" "title" "tags"   Add a note (tags comma-separated, optional)
  add note - "title" "tags"           Add a note from stdin
  add url "url" "title" "tags"        Add a URL bookmark
  list [limit]                   List recent entries (default 20)
  search "query" [limit]         Full-text search via FTS5 (default 20)
  pending                        List entries not yet compiled

Examples:
  kb ask "how does NFS work in the homelab?"
  kb add note "Docker tip" "Use restart: always" "docker,tips"
  kb add note - "My Note" "tag1" <<'EOF'
  ...long content...
  EOF
  kb add url "https://example.com" "Interesting article" "bookmarks"
  kb list 10
  kb search "docker"
  kb pending`)
}

// ─── ask ──────────────────────────────────────────────────────────────────────

func cmdAsk(ctx context.Context) {
	start := time.Now()

	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "Usage: kb ask \"question\"")
		os.Exit(1)
	}

	query := strings.Join(os.Args[2:], " ")

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

	indexData, err := os.ReadFile(wikiIndex)
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

func cmdAdd(ctx context.Context) {
	if len(os.Args) < 3 {
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

	entryType := os.Args[2]
	if entryType != "note" && entryType != "url" {
		fmt.Fprintf(os.Stderr, "Invalid type: %s (must be 'note' or 'url')\n", entryType)
		os.Exit(1)
	}

	var content, title, tags string

	if entryType == "note" && len(os.Args) > 3 && os.Args[3] == "-" {
		if len(os.Args) >= 5 {
			title = os.Args[4]
		}
		if len(os.Args) >= 6 {
			tags = os.Args[5]
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
		if len(os.Args) < 4 {
			fmt.Fprintf(os.Stderr, "Error: content is required for %s\n", entryType)
			os.Exit(1)
		}
		content = os.Args[3]
		if len(os.Args) >= 5 {
			title = os.Args[4]
		}
		if len(os.Args) >= 6 {
			tags = os.Args[5]
		}
	}

	if title == "" {
		title = firstLine(content, 80)
	}

	ts := time.Now().Format("2006-01-02T15:04:05")
	dateStr := time.Now().Format("2006-01-02")
	slug := slugify(title)
	if slug == "" {
		slug = slugify(content)
	}
	if slug == "" {
		slug = "untitled"
	}

	subdir := "notes"
	if entryType == "url" {
		subdir = "urls"
	}

	// UnixMilli suffix prevents filename collision when the same title is added twice on the same day
	rawPath := fmt.Sprintf("%s/%s/%s-%s-%d.md", rawDir, subdir, dateStr, slug, time.Now().UnixMilli())
	if err := os.MkdirAll(fmt.Sprintf("%s/%s", rawDir, subdir), 0755); err != nil {
		fmt.Fprintf(os.Stderr, "Error creating directory: %v\n", err)
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
	if err := os.WriteFile(rawPath, []byte(frontmatter), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing file: %v\n", err)
		os.Exit(1)
	}

	db, err := openDB()
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

func cmdList(ctx context.Context) {
	limit := 20
	if len(os.Args) >= 3 {
		if n, err := strconv.Atoi(os.Args[2]); err == nil && n > 0 {
			limit = n
		}
	}

	db, err := openDB()
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
	}
}

// ─── search ───────────────────────────────────────────────────────────────────

func cmdSearch(ctx context.Context) {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "Usage: kb search \"query\" [limit]")
		os.Exit(1)
	}

	query := parseFTSQuery(os.Args[2])
	if query == "" {
		fmt.Fprintln(os.Stderr, "Error: empty search query")
		os.Exit(1)
	}

	limit := 20
	if len(os.Args) >= 4 {
		if n, err := strconv.Atoi(os.Args[3]); err == nil && n > 0 {
			limit = n
		}
	}

	db, err := openDB()
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
	}

	if count == 0 {
		fmt.Println("No matches found.")
	}
}

// ─── pending ──────────────────────────────────────────────────────────────────

func cmdPending(ctx context.Context) {
	db, err := openDB()
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
	}

	if count == 0 {
		fmt.Println("All entries embedded.")
	} else {
		fmt.Printf("\n%d entries pending embedding.\nRun: python3 /opt/kb/compile.py\n", count)
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

// yamlQuote returns a YAML-safe representation of a string.
func yamlQuote(s string) string {
	if s == "" {
		return `""`
	}
	needsQuote := strings.ContainsAny(s, `:#"'`) ||
		strings.HasPrefix(s, " ") || strings.HasSuffix(s, " ") ||
		strings.HasPrefix(s, "-") || strings.HasPrefix(s, "?") ||
		strings.HasPrefix(s, "!") || strings.HasPrefix(s, "&") || strings.HasPrefix(s, "*") ||
		strings.HasPrefix(s, "{") || strings.HasPrefix(s, "}") ||
		strings.HasPrefix(s, "[") || strings.HasPrefix(s, "]") ||
		strings.HasPrefix(s, "@") || strings.HasPrefix(s, "`") ||
		strings.HasPrefix(s, ",") || strings.HasPrefix(s, "|") || strings.HasPrefix(s, ">") ||
		strings.Contains(s, "\n") || strings.Contains(s, ": ") ||
		s == "true" || s == "false" || s == "null" || s == "~" ||
		isNumericLike(s)
	if !needsQuote {
		return s
	}
	escaped := strings.ReplaceAll(s, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	escaped = strings.ReplaceAll(escaped, "\n", `\n`)
	return `"` + escaped + `"`
}

func isNumericLike(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && r != '.' && r != '-' && r != '+' && r != 'e' && r != 'E' {
			return false
		}
	}
	return true
}

// parseFTSQuery escapes an FTS5 query: if it contains special characters,
// treats it as a phrase (with escaped double quotes).
func parseFTSQuery(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if strings.ContainsAny(s, `*():"^+-`) {
		escaped := strings.ReplaceAll(s, `"`, `""`)
		return `"` + escaped + `"`
	}
	return s
}

// openDB opens SQLite with WAL and busy_timeout (prevents "database is locked" errors).
func openDB() (*sql.DB, error) {
	db, err := sql.Open("sqlite3", sqliteDSN)
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
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" {
			fmt.Print(chunk.Choices[0].Delta.Content)
		}
	}
	fmt.Println()
	return scanner.Err()
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
