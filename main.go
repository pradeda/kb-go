package main

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	_ "github.com/mattn/go-sqlite3"
)

const (
	dbPath           = "/opt/kb/kb.db"
	rawDir           = "/opt/kb/raw"
	wikiIndex        = "/opt/kb/wiki/index.md"
	openRouterURL    = "https://openrouter.ai/api/v1/chat/completions"
	defaultModel     = "google/gemini-2.5-flash-lite"
	chromaBase       = "http://localhost:8000/api/v2/tenants/default_tenant/databases/default_database"
	chromaCollection = "kb_collection"
	embedSocket      = "/run/kb-embed/embed.sock"
	maxChromaDist    = 0.40
)

var (
	httpQuick = &http.Client{Timeout: 15 * time.Second}
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
	loadEnv("/opt/kb/.env")

	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "ask":
		cmdAsk()
	case "add":
		cmdAdd()
	case "list":
		cmdList()
	case "search":
		cmdSearch()
	case "pending":
		cmdPending()
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

func cmdAsk() {
	start := time.Now()

	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "Usage: kb ask \"question\"")
		os.Exit(1)
	}

	query := strings.Join(os.Args[2:], " ")

	results, err := searchSemantic(query, start)
	strategy := "semantic"
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%v] Semantic error: %v\n",
			time.Since(start).Round(time.Millisecond), err)
	}
	if len(results) == 0 {
		fmt.Fprintf(os.Stderr, "[%v] No relevant results in KB\n",
			time.Since(start).Round(time.Millisecond))
	} else {
		fmt.Fprintf(os.Stderr, "[%v] Semantic search — %d results\n",
			time.Since(start).Round(time.Millisecond), len(results))
	}

	context := formatResults(results)
	fmt.Fprintf(os.Stderr, "--- DEBUG: Context ---\n%s\n--- END DEBUG ---\n", context)

	indexData, _ := os.ReadFile(wikiIndex)
	fmt.Fprintf(os.Stderr, "[%v] Index loaded (%d bytes)\n",
		time.Since(start).Round(time.Millisecond), len(indexData))

	prompt := fmt.Sprintf(`You are an assistant that answers solely based on content from a personal knowledge base.

RULES:
- Answer in detail and with specifics, using all relevant information from the context below.
- Cite source titles when possible.
- If the information is not present in the KB, explicitly say so — do not fabricate.
- Do not shorten the answer — the user wants as much detail as possible from the KB.
- Keep technical terms, project names, commands, and URLs in their original form — do not translate them.

---
RELEVANT KB RESULTS (strategy: %s):
%s

---
QUESTION: %s

---
Wiki index (for orientation only, not for direct answers):
%s`, strategy, context, query, string(indexData))

	if err := callOpenRouter(prompt, start); err != nil {
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

func cmdAdd() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "Usage: kb add <note|url> ...")
		fmt.Fprintln(os.Stderr, "  kb add note \"content\" \"title\" \"tags\"")
		fmt.Fprintln(os.Stderr, "  kb add note - \"title\" \"tags\"  (read content from stdin)")
		fmt.Fprintln(os.Stderr, "  kb add url \"url\" \"title\" \"tags\"")
		os.Exit(1)
	}

	entryType := os.Args[2]
	if entryType != "note" && entryType != "url" {
		fmt.Fprintf(os.Stderr, "Invalid type: %s (must be 'note' or 'url')\n", entryType)
		os.Exit(1)
	}

	var content, title, tags string

	if entryType == "note" && len(os.Args) > 3 && os.Args[3] == "-" {
		// Read from stdin
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
		// Use first line of content as title (truncated)
		title = firstLine(content, 80)
	}

	ts := time.Now().Format("2006-01-02T15:04:05")
	dateStr := time.Now().Format("2006-01-02")
	slug := slugify(title)
	if slug == "" {
		slug = slugify(content)
	}

	// Subdirectory per type
	subdir := "notes"
	if entryType == "url" {
		subdir = "urls"
	}

	rawPath := fmt.Sprintf("%s/%s/%s-%s.md", rawDir, subdir, dateStr, slug)
	if err := os.MkdirAll(fmt.Sprintf("%s/%s", rawDir, subdir), 0755); err != nil {
		fmt.Fprintf(os.Stderr, "Error creating directory: %v\n", err)
		os.Exit(1)
	}

	// Write raw markdown file with frontmatter
	frontmatter := fmt.Sprintf(`---
type: %s
content: %s
title: %s
tags: %s
saved: %s
---

%s
`, entryType, content, title, tags, ts, content)
	if err := os.WriteFile(rawPath, []byte(frontmatter), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing file: %v\n", err)
		os.Exit(1)
	}

	// Insert into SQLite
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	result, err := db.Exec(
		`INSERT INTO entries (type, content, title, tags, raw_path, source, created_at)
		 VALUES (?, ?, ?, ?, ?, 'cli', ?)`,
		entryType, content, title, tags, rawPath, ts,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error inserting into database: %v\n", err)
		os.Exit(1)
	}

	id, _ := result.LastInsertId()
	fmt.Printf("OK [id=%d] %s\n", id, rawPath)
}

// ─── list ─────────────────────────────────────────────────────────────────────

func cmdList() {
	limit := 20
	if len(os.Args) >= 3 {
		if n, err := strconv.Atoi(os.Args[2]); err == nil && n > 0 {
			limit = n
		}
	}

	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	rows, err := db.Query(
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
			continue
		}
		fmt.Printf("%-6d %-6s %-50s %s\n", id, typ, truncateASCII(title, 50), created)
	}
}

// ─── search ───────────────────────────────────────────────────────────────────

func cmdSearch() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "Usage: kb search \"query\" [limit]")
		os.Exit(1)
	}

	query := os.Args[2]
	limit := 20
	if len(os.Args) >= 4 {
		if n, err := strconv.Atoi(os.Args[3]); err == nil && n > 0 {
			limit = n
		}
	}

	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	rows, err := db.Query(
		`SELECT e.id, e.type, e.title, e.content, e.tags, e.created_at
		 FROM entries e
		 JOIN entries_fts fts ON e.id = fts.rowid
		 WHERE entries_fts MATCH ?
		 ORDER BY rank
		 LIMIT ?`, query, limit,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error searching: %v\n", err)
		os.Exit(1)
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var id int64
		var typ, title, content, tags, created string
		if err := rows.Scan(&id, &typ, &title, &content, &tags, &created); err != nil {
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

	if count == 0 {
		fmt.Println("No matches found.")
	}
}

// ─── pending ──────────────────────────────────────────────────────────────────

func cmdPending() {
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	rows, err := db.Query(
		`SELECT id, type, title, raw_path FROM entries
		 WHERE compiled_at IS NULL ORDER BY created_at`,
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
			continue
		}
		fmt.Printf("%-6d %-6s %-40s %s\n", id, typ, truncateASCII(title, 40), rawPath)
		count++
	}

	if count == 0 {
		fmt.Println("All entries compiled.")
	} else {
		fmt.Printf("\n%d entries pending compilation.\nRun: python3 /opt/kb/compile.py\n", count)
	}
}

// ─── helpers ──────────────────────────────────────────────────────────────────

func slugify(s string) string {
	s = strings.ToLower(s)
	var result strings.Builder
	lastDash := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			result.WriteRune(r)
			lastDash = false
		} else if !lastDash {
			result.WriteRune('-')
			lastDash = true
		}
	}
	out := strings.Trim(result.String(), "-")
	if len(out) > 60 {
		out = out[:60]
	}
	if out == "" {
		out = "untitled"
	}
	return out
}

func firstLine(s string, maxLen int) string {
	lines := strings.SplitN(strings.TrimSpace(s), "\n", 2)
	out := strings.TrimSpace(lines[0])
	if len(out) > maxLen {
		out = out[:maxLen] + "..."
	}
	if out == "" {
		out = "Untitled"
	}
	return out
}

func truncateASCII(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}

// ─── decay ranking ────────────────────────────────────────────────────────────

var dateFormats = []string{
	"2006-01-02T15:04:05",
	"2006-01-02",
	time.RFC3339,
}

const decayHalfLife = 180.0 // days
const minScoreThreshold = 0.15
const fallbackTopN = 3

func parseDate(s string) (time.Time, error) {
	for _, f := range dateFormats {
		if t, err := time.Parse(f, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized date format: %s", s)
}

func decayFactor(dateStr string) float64 {
	t, err := parseDate(dateStr)
	if err != nil {
		return 1.0 // unknown date: no penalty
	}
	daysOld := time.Since(t).Hours() / 24.0
	if daysOld < 0 {
		daysOld = 0
	}
	return 1.0 / (1.0 + daysOld/decayHalfLife)
}

func applyDecay(results []Result, idDist map[int64]float64) []Result {
	for i := range results {
		dist, ok := idDist[results[i].ID]
		if !ok {
			results[i].Score = 0
			continue
		}
		similarity := 1.0 - dist
		decay := decayFactor(results[i].Date)
		results[i].Score = similarity * decay

		daysOld := float64(0)
		if t, err := parseDate(results[i].Date); err == nil {
			daysOld = time.Since(t).Hours() / 24.0
		}
		fmt.Fprintf(os.Stderr, "[decay] id=%d dist=%.3f age=%.0fd decay=%.2f score=%.2f\n",
			results[i].ID, dist, daysOld, decay, results[i].Score)
	}

	// Sort descending by Score
	sortResults(results)

	// Filter below threshold; fallback to top N if all filtered out
	var filtered []Result
	for _, r := range results {
		if r.Score >= minScoreThreshold {
			filtered = append(filtered, r)
		}
	}
	if len(filtered) == 0 && len(results) > 0 {
		n := fallbackTopN
		if n > len(results) {
			n = len(results)
		}
		filtered = results[:n]
		fmt.Fprintf(os.Stderr, "[decay] all results below threshold %.2f, using top %d fallback\n",
			minScoreThreshold, n)
	}
	return filtered
}

func sortResults(results []Result) {
	for i := 0; i < len(results); i++ {
		for j := i + 1; j < len(results); j++ {
			if results[j].Score > results[i].Score {
				results[i], results[j] = results[j], results[i]
			}
		}
	}
}

// ─── OpenRouter ───────────────────────────────────────────────────────────────

func callOpenRouter(prompt string, start time.Time) error {
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

	req, err := http.NewRequest("POST", openRouterURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpSlow.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API error %d: %s", resp.StatusCode, string(b))
	}

	fmt.Fprintf(os.Stderr, "[%v] Stream started...\n", time.Since(start).Round(time.Millisecond))

	var chunk struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) > 0 {
			fmt.Print(chunk.Choices[0].Delta.Content)
		}
	}
	fmt.Println()
	return scanner.Err()
}

// ─── semantic search ──────────────────────────────────────────────────────────

func searchSemantic(query string, start time.Time) ([]Result, error) {
	fmt.Fprintf(os.Stderr, "[%v] Semantic search — embedding query...\n",
		time.Since(start).Round(time.Millisecond))

	embedding, err := getEmbedding(query)
	if err != nil {
		return nil, fmt.Errorf("embedding error: %v", err)
	}

	collectionID, err := getChromaCollectionID()
	if err != nil {
		return nil, fmt.Errorf("ChromaDB collection error: %v", err)
	}

	fmt.Fprintf(os.Stderr, "[%v] Querying ChromaDB...\n",
		time.Since(start).Round(time.Millisecond))

	payload, err := json.Marshal(map[string]any{
		"query_embeddings": [][]float64{embedding},
		"n_results":        10,
		"include":          []string{"distances"},
	})
	if err != nil {
		return nil, fmt.Errorf("json marshal: %v", err)
	}

	resp, err := httpQuick.Post(
		chromaBase+"/collections/"+collectionID+"/query",
		"application/json",
		bytes.NewReader(payload),
	)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ChromaDB error %d: %s", resp.StatusCode, string(b))
	}

	var chromaResp struct {
		IDs       [][]string  `json:"ids"`
		Distances [][]float64 `json:"distances"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&chromaResp); err != nil {
		return nil, err
	}

	if len(chromaResp.IDs) == 0 || len(chromaResp.IDs[0]) == 0 {
		return nil, nil
	}

	var sqliteIDs []string
	idDist := make(map[int64]float64)
	for i, dist := range chromaResp.Distances[0] {
		if dist > maxChromaDist {
			continue
		}
		id := chromaResp.IDs[0][i]
		if strings.HasPrefix(id, "gemini_") {
			continue
		}
		fmt.Fprintf(os.Stderr, "[%v] ChromaDB hit: id=%s distance=%.3f\n",
			time.Since(start).Round(time.Millisecond), id, dist)
		sqliteIDs = append(sqliteIDs, id)
	}

	if len(sqliteIDs) == 0 {
		fmt.Fprintf(os.Stderr, "[%v] ChromaDB — no relevant KB results\n",
			time.Since(start).Round(time.Millisecond))
		return nil, nil
	}

	results, err := fetchByIDs(sqliteIDs)
	if err != nil {
		return nil, err
	}

	// Build id → distance map for decay ranking
	for i, dist := range chromaResp.Distances[0] {
		if dist > maxChromaDist {
			continue
		}
		idStr := chromaResp.IDs[0][i]
		if strings.HasPrefix(idStr, "gemini_") {
			continue
		}
		if id, err := strconv.ParseInt(idStr, 10, 64); err == nil {
			idDist[id] = dist
		}
	}

	fmt.Fprintf(os.Stderr, "[%v] Applying time decay...\n",
		time.Since(start).Round(time.Millisecond))
	return applyDecay(results, idDist), nil
}

func fetchByIDs(ids []string) ([]Result, error) {
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	placeholders := strings.Repeat("?,", len(ids))
	placeholders = placeholders[:len(placeholders)-1]

	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}

	rows, err := db.Query(fmt.Sprintf(`
		SELECT id, title, content, summary, tags, type, created_at
		FROM entries WHERE id IN (%s)`, placeholders), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []Result
	for rows.Next() {
		var r Result
		if err := rows.Scan(&r.ID, &r.Title, &r.Content, &r.Summary, &r.Tags, &r.Source, &r.Date); err != nil {
			continue
		}
		results = append(results, r)
	}
	return results, nil
}

func getEmbedding(text string) ([]float64, error) {
	conn, err := net.Dial("unix", embedSocket)
	if err != nil {
		return nil, fmt.Errorf("kb-embed daemon not available (%s): %v", embedSocket, err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))

	if _, err := fmt.Fprintf(conn, "%s\n", text); err != nil {
		return nil, fmt.Errorf("sending text: %v", err)
	}

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 4*1024*1024), 4*1024*1024)
	if !scanner.Scan() {
		return nil, fmt.Errorf("empty response from daemon")
	}
	line := scanner.Text()
	if line == "null" {
		return nil, fmt.Errorf("daemon returned null (empty input or error)")
	}

	var embedding []float64
	if err := json.Unmarshal([]byte(line), &embedding); err != nil {
		return nil, fmt.Errorf("json parse: %v", err)
	}
	return embedding, nil
}

func getChromaCollectionID() (string, error) {
	resp, err := httpQuick.Get(chromaBase + "/collections/" + chromaCollection)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("ChromaDB error %d: %s", resp.StatusCode, string(b))
	}

	var result struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	return result.ID, nil
}

func formatResults(results []Result) string {
	if len(results) == 0 {
		return "(No direct hits)"
	}
	var sb strings.Builder
	for _, r := range results {
		title := r.Title
		if title == "" {
			title = "Untitled"
		}
		sb.WriteString(fmt.Sprintf("### %s [%s] (%s)\n", title, r.Source, r.Date))
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

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	runes := []rune(s)
	return string(runes[:n]) + "..."
}

func loadEnv(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			os.Setenv(parts[0], parts[1])
		}
	}
}
