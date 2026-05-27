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
	"strings"
	"time"
	"unicode/utf8"

	_ "github.com/mattn/go-sqlite3"
)

const (
	dbPath           = "/opt/kb/kb.db"
	wikiIndex        = "/opt/kb/wiki/index.md"
	openRouterURL    = "https://openrouter.ai/api/v1/chat/completions"
	model            = "google/gemini-2.5-flash-lite"
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
	Title   string
	Content string
	Summary string
	Tags    string
	Source  string
	Date    string
}

func main() {
	start := time.Now()
	loadEnv("/opt/kb/.env")

	if len(os.Args) < 2 {
		fmt.Println("Usage: kb-ask 'your question'")
		os.Exit(1)
	}

	query := strings.Join(os.Args[1:], " ")

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

func callOpenRouter(prompt string, start time.Time) error {
	apiKey := os.Getenv("OPENROUTER_API_KEY")
	if apiKey == "" {
		return fmt.Errorf("OPENROUTER_API_KEY not set")
	}

	modelToUse := os.Getenv("OPENROUTER_MODEL")
	if modelToUse == "" {
		modelToUse = model
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
	// Increased buffer from default 64KB to 1MB — prevents ErrTooLong on long responses
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

	return fetchByIDs(sqliteIDs)
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
		SELECT title, content, summary, tags, type, created_at
		FROM entries WHERE id IN (%s)`, placeholders), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []Result
	for rows.Next() {
		var r Result
		if err := rows.Scan(&r.Title, &r.Content, &r.Summary, &r.Tags, &r.Source, &r.Date); err != nil {
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
		return nil, fmt.Errorf("slanje teksta: %v", err)
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
