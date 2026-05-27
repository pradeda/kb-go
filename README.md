# kb-go

Homelab knowledge base CLI — semantic search + note management backed by ChromaDB, SQLite (FTS5), and FastEmbed.

## Architecture

```
kb-ask "question"
  ├── FastEmbed daemon (Unix socket, ~50ms) — embed query
  ├── ChromaDB (cosine distance ≤ 0.40) — top 10 semantic matches
  ├── SQLite (FTS5) — fetch full content by IDs
  └── OpenRouter LLM — synthesize answer from chunks
```

## Requirements

- Go 1.24+
- SQLite with FTS5 support (`go build -tags fts5`)
- External services:
  - FastEmbed daemon at `/run/kb-embed/embed.sock`
  - ChromaDB at `localhost:8000`
  - SQLite DB at `/opt/kb/kb.db`
  - OpenRouter API key in `/opt/kb/.env`

## Build

```bash
make build    # builds kb-go binary
make install  # builds + installs to /usr/local/bin/kb-ask
```

Manual:

```bash
go build -tags fts5 -o kb-go .
sudo cp kb-go /usr/local/bin/kb-ask
```

## Usage

```bash
kb-ask "how does NFS work in the homelab?"
```

## Config

`/opt/kb/.env`:
```
OPENROUTER_API_KEY=sk-or-v1-...
OPENROUTER_MODEL=google/gemini-2.5-flash-lite
```
