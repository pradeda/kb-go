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

kb ask --scope homelab|ai|both|auto "question"
  ├── POST /v2/kb/search with Bearer auth and allow_degraded=false
  ├── Fixed-shape, corpus-grouped results with qualified refs (homelab:1 / ai:1)
  ├── Homelab wiki index only when Homelab is selected
  └── OpenRouter LLM — synthesize while keeping corpus evidence distinct

kb add note "content" "title" "tags"
  ├── Secret scanner (Gate 1) — redact credentials before they are stored
  ├── Write raw markdown file (frontmatter + body)
  └── Insert into SQLite entries table (FTS5 auto-indexed via triggers)
       │
       └── kb-watcher (inotify, 5s debounce) → compile.py
             ├── Secret scanner (Gate 2) — embed-time safety-net over entries.content
             └── ChromaDB

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
  - Homelab SQLite DB at `/opt/kb/kb.db`
  - AI SQLite DB at `/opt/ai-kb/ai-kb.db`

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

AI storage provisioning is an explicit, fixed-target operation. It initializes the
`entries`/FTS5 schema, merge-migrates Homelab collection metadata without dropping
`hnsw:space=cosine`, and creates `ai_kb_collection`:

```bash
sudo /usr/bin/python3 provision_storage.py --apply
/usr/bin/python3 provision_storage.py --health
```

Run it only after a restore-tested backup. `--health` is read-only and fails on a
missing collection or corpus/model/dimension/metric mismatch.

## Usage

### Semantic search (`kb ask`)

```bash
kb ask "how does NFS work in the homelab?"
kb ask --scope ai "what does the research corpus say about local models?"
kb ask --scope both "compare deployed state with research notes"
```

Requires: OpenRouter API key in `/opt/kb/.env`, KB Search API running on `:8050`.
Scoped calls additionally require `KB_V2_TOKEN_KB_CLI_LOCAL` in the same private
environment file. Legacy calls without `--scope` remain byte-compatible and
Homelab-only. `--scope auto` intentionally returns an error until the calibrated
router is activated in a later phase.

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
- **Streaming integrity**: OpenRouter SSE output is accepted only after `[DONE]`; malformed, provider-error, or prematurely closed streams fail instead of returning a silently truncated answer

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

# Explicit AI corpus (write target must precede positional arguments)
kb add --corpus ai note "Transformer note" "Attention overview" "ai,ml"
```

Legacy commands without `--corpus` remain Homelab-only. The only accepted write targets are `homelab` and `ai`; arbitrary database, raw, or collection paths are not accepted. The separate `kb-watcher` and `ai-kb-watcher` services run the same allowlisted compiler with different raw roots, Chroma collections, locks, and state files.

Raw entries are created atomically (`O_CREATE|O_EXCL`) so concurrent same-title writes cannot overwrite each other. Files use mode `0600` and their directories use `0700` because raw Markdown contains the same potentially sensitive content as `kb.db`.

#### Secret scanner (credential leak prevention)

Because automated ingest (LLMs via the MCP `add` tool, pipelines, monitoring notes) can accidentally write tokens, API keys, or passwords into a note, every write is scanned and secrets are redacted in place **before** the content becomes searchable. Two gates share one ruleset — `/opt/kb/secret_patterns.json` (RE2 subset, so the Go and Python sides redact identically):

- **Gate 1** (`secretscan.go`, in `kb add`): runs synchronously before the SQLite `INSERT`. This is the important one — `kb add` writes to `entries` immediately, so content is FTS-searchable before it is ever embedded.
- **Gate 2** (`compile.py`, `sanitize_unembedded`): an embed-time safety-net over `entries.content`, for any row that reached the DB without going through `kb add` (direct raw drop, rsync, future tools). An `UPDATE` propagates to FTS via triggers; the on-disk raw archive is redacted too.

On a hit the value is replaced with a typed placeholder (`<REDACTED_OPENROUTER_KEY>`, `<REDACTED_SECRET>`, …), the original is backed up to `/opt/kb/quarantine/<id>-<ts>.orig` (`0600`), and one line is appended to `/opt/kb/quarantine.log`. There is no review queue and no notification — a blocked secret simply never enters the KB.

Patterns are tiered: **value-shaped Tier 1** (provider prefixes like `sk-or-v1-`, `sk-ant-`, `ghp_`; URL basic-auth `scheme://user:pass@`; `PASSWORD=`/`secret=` assignments) auto-redact on match; a **Tier 2 generic keyword-assignment** pattern is log-only behind a Shannon-entropy gate. An `allowlist` (exact) plus `allow_contains` (substring) suppress false positives such as `SONARR_API_KEY`, `KEY`, and placeholder passwords. Adding coverage for a new service is a one-line edit to `secret_patterns.json` — both gates read it at runtime, no rebuild needed.

> **Gotcha:** do not put `\b` before a secret keyword in the keyword patterns — `_` is a word char, so `\bpassword` fails to match `FOO_PASSWORD` / `SONARR_API_KEY=...`, the most common leak form. Covered by `secretscan_test.go`.

`compile.py` previously also generated wiki pages via OpenRouter LLM synthesis — that step is currently **disabled** (commented out in `main()`) pending future wiki reactivation. Only ChromaDB embedding runs by default.

Recovery flags (for disaster scenarios):
```bash
python3 /opt/kb/compile.py --recover-db    # raw .md → rebuild SQLite
python3 /opt/kb/compile.py --recover-raw   # SQLite → regenerate raw .md
```

Titles and tags use JSON-quoted strings, which are valid YAML scalars. Both recovery directions share the same encoder/decoder, preserve quotes and control characters, and remain compatible with legacy unquoted raw files.

**Rebuilding the vector index** (ChromaDB data lost — e.g. container recreated with a wrong mount): embeddings are derived data, source of truth is `kb.db` + `raw/`. Set `UPDATE entries SET embedded_at=NULL`, then re-embed in **small slices (~15 entries per fresh process)** — running `compile.py` once over hundreds of entries OOM-kills on a 16GB host (nomic-embed with 8k-token sequences). ChromaDB compose must mount `./chroma_data:/data` (rust Chroma ignores `PERSIST_DIRECTORY` and always writes to `/data`).

LLM output file paths in `parse_and_write()` are validated with `resolve()` + `is_relative_to()` — a hallucinated absolute or `../` path cannot write outside `/opt/kb`.

### Auto-compile watcher

```bash
sudo cp kb-watcher.service /etc/systemd/system/
sudo cp ai-kb-watcher.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now kb-watcher ai-kb-watcher
```

The watchers monitor `/opt/kb/raw/` and `/opt/ai-kb/raw/` independently, debounce for 5s, and run `compile.py` for their fixed corpus.

Uses a **blocking** `flock` (not `flock -n`): an event that arrives while a compile is already running waits for its own run instead of being silently dropped — otherwise the entry would stay unembedded (invisible to semantic search) until some future event triggered another compile.

### Full-text search (`kb search`)

```bash
kb search "docker"          # default 20 results
kb search "nginx" 10        # limit to 10 results
kb search "192.168.1.174"   # punctuation-safe literal search
kb search "/opt/kb"         # paths are safe too
kb search --corpus ai "transformers" 10
```

Uses SQLite FTS5. Each whitespace-delimited term is treated as a literal and terms are combined with implicit AND semantics. Advanced FTS operators (`OR`, `NOT`, `*`, and similar syntax) are intentionally not interpreted. Does NOT require ChromaDB or OpenRouter.

### List entries (`kb list`)

```bash
kb list           # last 20 entries
kb list 50        # last 50 entries
kb list --corpus ai 20
```

### Pending embedding (`kb pending`)

```bash
kb pending        # entries not yet embedded in ChromaDB
kb pending --corpus ai
```

## Config

Each corpus has its own private env file (`/opt/kb/.env` and `/opt/ai-kb/.env`):
```
OPENROUTER_API_KEY=sk-or-v1-...
OPENROUTER_MODEL=google/gemini-2.5-flash-lite
```

## Related services

| Service | Path | Port/Unit |
|---------|------|-----------|
| KB Search API | `/opt/kb/kb_search_api.py` | `:8050` (systemd: `kb-search-api`) |
| MCP server | `/opt/kb/mcp_server.py` | registered in Claude Code and Codex |
| kb-watcher | `/opt/kb/watcher.sh` | systemd: `kb-watcher` |
| FastEmbed daemon | `/opt/kb/embed_daemon.py` | systemd: `kb-embed` |
| ChromaDB | Docker: `kb-chromadb` (image pinned, no watchtower) | `:8000` (localhost only) |

## Tests

Run the canonical suite through the Makefile so the SQLite driver is compiled with FTS5 support:

```bash
make test
```

This runs `go test -tags sqlite_fts5 ./...` plus the compiler, provisioning, metadata, and watcher contract tests. Plain `go test ./...` does not enable the FTS5 module and is not the supported test command.
