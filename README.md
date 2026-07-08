# kb

Homelab knowledge base CLI — semantic search, note management, and full-text search backed by KB Search API, ChromaDB, SQLite (FTS5), and FastEmbed.

> **Depends on:** [kb-mcp](https://github.com/pradeda/kb-mcp) — provides the KB Search API (`kb_search_api.py`) that `kb ask` calls for retrieval + cross-encoder reranking.

## Architecture

```
kb ask "question"
  ├── POST /kb/search (KB Search API, :8050)
  │     ├── FastEmbed daemon (Unix socket, ~50ms) — embed query
  │     ├── ChromaDB (cosine distance ≤ 0.60) — top 25 candidates (broad recall)
  │     ├── SQLite — fetch full content + metadata by IDs
  │     ├── Cross-encoder (ms-marco-MiniLM-L-6-v2, CPU) — rerank with sigmoid relevance
  │     ├── Dedup — keep best chunk per entry
  │     ├── Time decay — final = relevance × max(1/(1+days/540), 0.3)
  │     └── Threshold 0.40 + cap top 5
  └── OpenRouter LLM — synthesize answer from enriched chunks

kb add note "content" "title" "tags"
  ├── Write raw markdown file (frontmatter + body)
  └── Insert into SQLite entries table (FTS5 auto-indexed via triggers)
       │
       └── kb-watcher (inotify, 5s debounce) → compile.py → ChromaDB

kb search "query"
  └── SQLite FTS5 full-text search (entries_fts)

kb pending
  └── Shows entries not yet embedded in ChromaDB (embedded_at IS NULL)
```

### Ranking pipeline (implemented in `kb_search_api.py`)

```
Layer 1: ChromaDB cosine → top 25 (broad recall)
Layer 2: Cross-encoder rerank → sigmoid → [0,1] relevance + dedup
Layer 3: Time decay → final = relevance × max(1/(1+days/540), 0.3)
Layer 4: Threshold 0.40 → cap top 5 (full) / top 3 (websearch)
         No fallback — empty response is an honest signal.
```

## Requirements

- Go 1.24+
- Python 3.10+ with `sentence-transformers` (cross-encoder model)
- SQLite with FTS5 support (`go build -tags fts5`)
- External services:
  - KB Search API at `http://192.168.1.174:8050` (systemd: `kb-search-api`)
  - FastEmbed daemon at `/run/kb-embed/embed.sock`
  - ChromaDB at `localhost:8000`
  - SQLite DB at `/opt/kb/kb.db`

## Build

```bash
make build    # builds kb binary
make install  # builds + installs kb to /usr/local/bin/ and compile.py to /opt/kb/
```

Manual:

```bash
go build -tags fts5 -o kb .
sudo cp kb /usr/local/bin/kb
sudo cp compile.py /opt/kb/compile.py
```

## Usage

### Semantic search (`kb ask`)

```bash
kb ask "how does NFS work in the homelab?"
```

Requires: OpenRouter API key in `/opt/kb/.env`, KB Search API running on `:8050`.

Results are ranked by a 4-layer pipeline (see Architecture above). The final score combines cross-encoder relevance with time decay:

```
final = sigmoid(cross_encoder(query, chunk)) × max(1/(1 + days_old/540), 0.3)
```

- **Cross-encoder**: `ms-marco-MiniLM-L-6-v2`, ~80MB, runs on CPU (~200-400ms for 25 candidates)
- **Half-life**: 540 days (~1.5yr) — conservative for homelab technical docs
- **Decay floor**: 0.3 — entry never drops below 30% weight
- **Threshold**: 0.40 — results below this are discarded (no fallback)
- **Cap**: top 5 results passed to LLM
- **Empty response**: LLM told "no relevant results" — does NOT fabricate

### Add entries (`kb add`)

```bash
# Short note
kb add note "Docker tip" "Use restart: always" "docker,tips"

# Long note from stdin (heredoc)
kb add note - "Configuration Guide" "config,linux" <<'EOF'
Full configuration details here...
Multiple lines supported.
EOF

# URL bookmark
kb add url "https://example.com" "Interesting article" "bookmarks"
```

After adding entries, the `kb-watcher` systemd service automatically runs `/opt/kb/compile.py`, which embeds them into ChromaDB for semantic search. No manual step needed.

`compile.py` previously also generated wiki pages via OpenRouter LLM synthesis — that step is currently **disabled** (commented out in `main()`) pending future wiki reactivation. Only ChromaDB embedding runs by default.

Recovery flags (for disaster scenarios):
```bash
python3 /opt/kb/compile.py --recover-db    # raw .md → rebuild SQLite
python3 /opt/kb/compile.py --recover-raw   # SQLite → regenerate raw .md
```

**Rebuilding the vector index** (ChromaDB data lost — e.g. container recreated with a wrong mount): embeddings are derived data, source of truth is `kb.db` + `raw/`. Set `UPDATE entries SET embedded_at=NULL`, then re-embed in **small slices (~15 entries per fresh process)** — running `compile.py` once over hundreds of entries OOM-kills on a 16GB host (nomic-embed with 8k-token sequences). ChromaDB compose must mount `./chroma_data:/data` (rust Chroma ignores `PERSIST_DIRECTORY` and always writes to `/data`).

LLM output file paths in `parse_and_write()` are validated with `resolve()` + `is_relative_to()` — a hallucinated absolute or `../` path cannot write outside `/opt/kb`.

### Auto-compile watcher

```bash
sudo cp kb-watcher.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now kb-watcher
```

Watches `/opt/kb/raw/` via inotify, debounces 5s, runs `compile.py` automatically. No manual step needed.

Uses a **blocking** `flock` (not `flock -n`): an event that arrives while a compile is already running waits for its own run instead of being silently dropped — otherwise the entry would stay unembedded (invisible to semantic search) until some future event triggered another compile.

### Full-text search (`kb search`)

```bash
kb search "docker"          # default 20 results
kb search "nginx" 10        # limit to 10 results
```

Uses SQLite FTS5. Does NOT require ChromaDB or OpenRouter.

### List entries (`kb list`)

```bash
kb list           # last 20 entries
kb list 50        # last 50 entries
```

### Pending embedding (`kb pending`)

```bash
kb pending        # entries not yet embedded in ChromaDB
```

## Config

`/opt/kb/.env`:
```
OPENROUTER_API_KEY=sk-or-v1-...
OPENROUTER_MODEL=google/gemini-2.5-flash-lite
```

## Related services

| Service | Path | Port/Unit |
|---------|------|-----------|
| KB Search API | `/opt/kb/kb_search_api.py` | `:8050` (systemd: `kb-search-api`) |
| MCP server | `/opt/kb/mcp_server.py` | registered in Claude Code |
| kb-watcher | `/opt/kb/watcher.sh` | systemd: `kb-watcher` |
| FastEmbed daemon | `/opt/kb/embed_daemon.py` | systemd: `kb-embed` |
| ChromaDB | Docker: `kb-chromadb` (image pinned, no watchtower) | `:8000` (localhost only) |
