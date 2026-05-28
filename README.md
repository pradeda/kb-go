# kb

Homelab knowledge base CLI — semantic search, note management, and full-text search backed by ChromaDB, SQLite (FTS5), and FastEmbed.

## Architecture

```
kb ask "question"
  ├── FastEmbed daemon (Unix socket, ~50ms) — embed query
  ├── ChromaDB (cosine distance ≤ 0.40) — top 10 semantic matches
  ├── SQLite (FTS5) — fetch full content by IDs
  ├── Time decay ranking — deprioritize stale entries (half-life: 180 days)
  └── OpenRouter LLM — synthesize answer from chunks

kb add note "content" "title" "tags"
  ├── Write raw markdown file (frontmatter + body)
  └── Insert into SQLite entries table (FTS5 auto-indexed via triggers)

kb search "query"
  └── SQLite FTS5 full-text search (entries_fts)

kb pending → compile.py
  └── Shows entries not yet compiled → run compile.py to embed in ChromaDB
```

## Requirements

- Go 1.24+
- SQLite with FTS5 support (`go build -tags fts5`)
- External services:
  - FastEmbed daemon at `/run/kb-embed/embed.sock`
  - ChromaDB at `localhost:8000`
  - SQLite DB at `/opt/kb/kb.db`

## Build

```bash
make build    # builds kb binary
make install  # builds + installs to /usr/local/bin/kb
```

Manual:

```bash
go build -tags fts5 -o kb .
sudo cp kb /usr/local/bin/kb
```

## Usage

### Semantic search (`kb ask`)

```bash
kb ask "how does NFS work in the homelab?"
```

Requires: OpenRouter API key in `/opt/kb/.env`

Results are ranked by a combined score:

```
score = (1.0 - cosine_distance) × decay_factor
decay_factor = 1 / (1 + age_in_days / 180)
```

- **Half-life**: 180 days — a 6-month-old entry retains 50% weight
- **Threshold**: entries scoring below 0.15 are filtered out
- **Fallback**: if every result falls below threshold, top 3 are kept regardless
- **Date parsing**: supports `2006-01-02T15:04:05`, `2006-01-02`, and RFC3339 formats
- **Debug output** shows per-entry decay on stderr: `[decay] id=183 dist=0.27 age=10d decay=0.95 score=0.69`

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

After adding entries, `compile.py` embeds them into ChromaDB for semantic search via the auto-compile watcher.

`compile.py` previously also generated wiki pages via OpenRouter LLM synthesis — that step is currently **disabled** (commented out in `main()`) pending future wiki reactivation. Only ChromaDB embedding runs by default.

### Auto-compile watcher

```bash
sudo cp kb-watcher.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now kb-watcher
```

Watches `/opt/kb/raw/` via inotify, debounces 5s, runs `compile.py` automatically. No manual step needed.

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

### Pending compilation (`kb pending`)

```bash
kb pending        # entries not yet compiled
```

## Config

`/opt/kb/.env`:
```
OPENROUTER_API_KEY=sk-or-v1-...
OPENROUTER_MODEL=google/gemini-2.5-flash-lite
```
