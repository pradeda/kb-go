package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

type v2SearchResult struct {
	Corpus          string  `json:"corpus"`
	EntryID         int64   `json:"entry_id"`
	Ref             string  `json:"ref"`
	Title           string  `json:"title"`
	Content         *string `json:"content"`
	Tags            *string `json:"tags"`
	PublicSourceURL *string `json:"public_source_url"`
	Link            string  `json:"link"`
	Distance        float64 `json:"distance"`
	Relevance       float64 `json:"relevance"`
	FinalScore      float64 `json:"final_score"`
}

type v2CorpusResults struct {
	Searched  bool             `json:"searched"`
	Available bool             `json:"available"`
	Count     int              `json:"count"`
	Results   []v2SearchResult `json:"results"`
}

type v2SearchResponse struct {
	Query              string                     `json:"query"`
	RequestedScope     string                     `json:"requested_scope"`
	SelectedScope      string                     `json:"selected_scope"`
	RoutingMode        string                     `json:"routing_mode"`
	RoutingReason      string                     `json:"routing_reason"`
	NeedsClarification bool                       `json:"needs_clarification"`
	RouterVersion      *string                    `json:"router_version"`
	DegradedCorpora    []string                   `json:"degraded_corpora"`
	TotalCount         int                        `json:"total_count"`
	Corpora            map[string]v2CorpusResults `json:"corpora"`
}

func cmdAskV2(ctx context.Context, query, scope string, profile CorpusProfile, start time.Time) error {
	response, err := searchViaAPIV2(ctx, query, scope, start)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%v] KB Search API v2 error: %v\n", time.Since(start).Round(time.Millisecond), err)
		fmt.Fprintf(os.Stderr, "[%v] Search unavailable — cannot answer.\n", time.Since(start).Round(time.Millisecond))
		return err
	}
	if response.TotalCount == 0 {
		fmt.Fprintf(os.Stderr, "[%v] KB Search API v2 returned 0 results.\n", time.Since(start).Round(time.Millisecond))
	} else {
		fmt.Fprintf(os.Stderr, "[%v] KB Search API v2 — %d grouped results after rerank\n", time.Since(start).Round(time.Millisecond), response.TotalCount)
	}

	contextText := formatV2Results(response)
	if os.Getenv("KB_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "--- DEBUG: Context ---\n%s\n--- END DEBUG ---\n", contextText)
	}

	indexText := ""
	if response.SelectedScope == "homelab" || response.SelectedScope == "both" {
		indexData, readErr := os.ReadFile(profile.WikiIndexPath)
		if readErr != nil {
			fmt.Fprintf(os.Stderr, "[%v] warn: wiki index missing (%v)\n", time.Since(start).Round(time.Millisecond), readErr)
		} else {
			indexText = string(indexData)
			fmt.Fprintf(os.Stderr, "[%v] Index loaded (%d bytes)\n", time.Since(start).Round(time.Millisecond), len(indexData))
		}
	}

	prompt := fmt.Sprintf(`You are an assistant that answers solely based on content from personal knowledge-base corpora.

RULES:
- Answer in detail and with specifics, using all relevant information from the context below.
- Cite corpus-qualified references such as homelab:521 or ai:17; never cite an unqualified numeric ID.
- Keep evidence from different corpora distinguishable and do not imply that AI research notes describe deployed Homelab state.
- If the information is not present, explicitly say so — do not fabricate.
- If the context below is empty or says "No relevant results", respond with: "I couldn't find relevant information in the knowledge base for this query."
- Keep technical terms, project names, commands, and URLs in their original form — do not translate them.
- Results are tagged HIGH/MEDIUM/LOW relevance — prefer HIGH over LOW; treat LOW as weak evidence.

---
GROUPED KB RESULTS (scope: %s, selected: %s):
%s

---
QUESTION: %s

---
Homelab wiki index (orientation only; empty when Homelab was not selected):
%s`, response.RequestedScope, response.SelectedScope, contextText, query, indexText)

	if err := callOpenRouter(ctx, prompt, start); err != nil {
		fmt.Fprintf(os.Stderr, "[%v] OpenRouter did not respond: %v\n", time.Since(start).Round(time.Millisecond), err)
		fmt.Fprintf(os.Stderr, "[%v] Here are raw KB results:\n", time.Since(start).Round(time.Millisecond))
		fmt.Println(contextText)
		return err
	}
	fmt.Fprintf(os.Stderr, "[%v] Done.\n", time.Since(start).Round(time.Millisecond))
	return nil
}

func searchViaAPIV2(ctx context.Context, query, scope string, start time.Time) (v2SearchResponse, error) {
	fmt.Fprintf(os.Stderr, "[%v] Calling KB Search API v2...\n", time.Since(start).Round(time.Millisecond))
	token := os.Getenv("KB_V2_TOKEN_KB_CLI_LOCAL")
	if token == "" {
		return v2SearchResponse{}, fmt.Errorf("KB_V2_TOKEN_KB_CLI_LOCAL not set")
	}
	body, err := json.Marshal(map[string]any{
		"query":          query,
		"scope":          scope,
		"top_k":          5,
		"allow_degraded": false,
	})
	if err != nil {
		return v2SearchResponse{}, fmt.Errorf("json marshal: %v", err)
	}

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return v2SearchResponse{}, err
		}
		if attempt > 0 {
			backoff := time.Duration(500*(1<<(attempt-1))) * time.Millisecond
			fmt.Fprintf(os.Stderr, "[%v] Retry %d/3 (backoff %v)...\n", time.Since(start).Round(time.Millisecond), attempt+1, backoff)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return v2SearchResponse{}, ctx.Err()
			}
		}
		req, requestErr := http.NewRequestWithContext(ctx, http.MethodPost, kbSearchAPIV2, bytes.NewReader(body))
		if requestErr != nil {
			lastErr = requestErr
			continue
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, requestErr := httpQuick.Do(req)
		if requestErr != nil {
			lastErr = requestErr
			continue
		}
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			message, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			return v2SearchResponse{}, fmt.Errorf("API error %d (non-retryable): %s", resp.StatusCode, string(message))
		}
		if resp.StatusCode != http.StatusOK {
			message, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			lastErr = fmt.Errorf("API error %d: %s", resp.StatusCode, string(message))
			continue
		}
		var value v2SearchResponse
		decodeErr := json.NewDecoder(resp.Body).Decode(&value)
		resp.Body.Close()
		if decodeErr != nil {
			lastErr = decodeErr
			continue
		}
		if _, ok := value.Corpora["homelab"]; !ok {
			return v2SearchResponse{}, fmt.Errorf("invalid v2 response: homelab corpus missing")
		}
		if _, ok := value.Corpora["ai"]; !ok {
			return v2SearchResponse{}, fmt.Errorf("invalid v2 response: ai corpus missing")
		}
		return value, nil
	}
	return v2SearchResponse{}, fmt.Errorf("KB Search API v2 unavailable after 3 attempts: %v", lastErr)
}

func formatV2Results(response v2SearchResponse) string {
	if response.TotalCount == 0 {
		return "(No relevant KB results found for this query.)"
	}
	var output strings.Builder
	for _, corpus := range []string{"homelab", "ai"} {
		group := response.Corpora[corpus]
		if !group.Searched {
			continue
		}
		output.WriteString(fmt.Sprintf("## Corpus: %s\n\n", corpus))
		for _, result := range group.Results {
			title := result.Title
			if title == "" {
				title = "Untitled"
			}
			output.WriteString(fmt.Sprintf("### %s [%s] (%s) — %s\n", title, result.Ref, result.Link, relevanceLabel(result.FinalScore)))
			if result.Tags != nil && *result.Tags != "" {
				output.WriteString(fmt.Sprintf("Tags: %s\n", *result.Tags))
			}
			if result.Content != nil {
				output.WriteString(truncate(*result.Content, 3000))
			}
			output.WriteString("\n\n---\n\n")
		}
	}
	return output.String()
}
