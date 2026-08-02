# KB Chunking and Reranker Migration Plan

Status: planned, not implemented  
Scope: `kb-go`, `kb-mcp`, live `/opt/kb` services  
Primary goal: replace one-entry/one-vector retrieval with chunk-level recall and reranking without risking the current KB  
Source of truth: SQLite `/opt/kb/kb.db`; ChromaDB remains disposable derived data

## Why this change is needed

The current pipeline does not chunk KB entries:

```text
SQLite entry
  -> title + summary + content[:8000] + tags
  -> one embedding
  -> one Chroma record whose ID is the SQLite entry ID
  -> cross-encoder sees only content[:1500]
```

This creates two independent blind spots:

1. Recall blind spot: relevant text after `content[:8000]` is absent from the embedding, so ChromaDB may never return the entry.
2. Reranker blind spot: even when ChromaDB returns the entry, relevant text after `content[:1500]` is invisible to the cross-encoder.

Changing only the reranker model or threshold cannot fix either blind spot.

## Non-goals

- Do not change SQLite into a chunk store.
- Do not make ChromaDB a source of truth.
- Do not replace the current cross-encoder before measuring chunking with the existing model.
- Do not delete or rebuild `kb_collection` in place.
- Do not change LLM synthesis or remove it from the MCP workflow.
- Do not reset `entries.embedded_at` during the initial v2 build; v1 must remain operational.

## Target architecture

```text
SQLite entry #409
  -> Markdown-aware chunker
     -> entry:409:chunk:0
     -> entry:409:chunk:1
     -> entry:409:chunk:2
  -> kb_collection_v2
  -> Chroma recall: top chunk candidates
  -> cross-encoder reranks query/chunk pairs
  -> group by entry_id and keep best chunk per entry
  -> time decay by entry date
  -> relevance threshold
  -> top 5 entries (full) / top 3 entries (websearch)
  -> return matched_chunk to consumers
```

SQLite continues to store one canonical row per KB entry. Chunk boundaries and embeddings are derived and can always be regenerated.

## Chroma v2 record contract

Use a separate collection during development and rollout:

```text
kb_collection_v2
```

Recommended record ID:

```text
entry:<entry_id>:chunk:<chunk_index>:<content_hash_prefix>
```

Recommended metadata:

```json
{
  "schema_version": 2,
  "entry_id": "409",
  "chunk_index": 2,
  "chunk_count": 5,
  "content_hash": "sha256-of-canonical-entry-content",
  "title": "Entry title",
  "tags": "kb,docker",
  "raw_path": "/opt/kb/raw/notes/example.md",
  "created_at": "2026-07-11T12:00:00"
}
```

The Chroma `document` must contain the exact text embedded and later reranked:

```text
Title: <title>
Summary: <summary, if present>
Tags: <tags, if present>

<chunk body>
```

The content hash in the ID allows a new version of an entry to be upserted before stale chunks are deleted. This avoids a temporary search gap during updates.

## Chunking rules

Start with conservative defaults and make them constants:

```text
CHUNK_TARGET_TOKENS = 350
CHUNK_OVERLAP_TOKENS = 60
CHUNK_MAX_TOKENS = 450
CHUNK_SCHEMA_VERSION = 2
```

Required behavior:

1. Split first on Markdown headings.
2. Within a section, pack complete paragraphs until the target size is reached.
3. Keep fenced code blocks intact when they fit within the hard maximum.
4. Split oversized paragraphs/code blocks only as a final fallback.
5. Add overlap from the previous chunk without duplicating the metadata prefix.
6. Never emit an empty chunk.
7. Preserve original text; do not summarize during chunk generation.
8. Use the embedding model tokenizer if it is available. A character-count approximation is acceptable only as an explicitly tested fallback.

Special cases:

- Empty entry: skip it, report it, and do not leave it permanently pending.
- Very short entry: produce one chunk.
- Title-only or URL-only entry: include title/tags and the available body as one chunk.
- Large code/config entry: prefer structural boundaries and enforce the hard token maximum.
- Invalid UTF-8 is already normalized before this stage; chunking operates on Python strings.

## Phase 0 — establish a baseline

Do this before changing retrieval behavior.

- [ ] Create a versioned eval fixture containing at least 30–50 real queries.
- [ ] Include Serbian, English, mixed-language, single-word, command, IP, path, and no-answer queries.
- [ ] For each query record expected SQLite entry IDs and optionally the expected evidence text.
- [ ] Include known edge cases such as `whatsapp`, NFS/CIFS incidents, KB ports, Qwen/Chroma, Watchtower, and exact config paths.
- [ ] Capture current v1 output: candidate IDs, cosine distances, reranker relevance, final score, rank, and latency.
- [ ] Measure recall@25 before reranking, MRR/nDCG after reranking, precision@5, false-empty rate, false-positive rate on no-answer queries, p50 latency, and p95 latency.
- [ ] Store eval code and fixtures in a repository, not only in `/tmp`.

Do not tune thresholds using only one or two queries.

## Phase 1 — implement and test the chunker in `kb-go`

Target repository: `/home/turok/projects/kb-go`

Likely files:

- `compile.py` — incremental v2 embedding
- new `chunking.py` — pure chunk-generation functions
- new Python tests for chunk boundaries and metadata
- optional new `reindex_chunks.py` — safe full build of `kb_collection_v2`

Checklist:

- [ ] Implement the chunker as pure functions with no DB or Chroma side effects.
- [ ] Add tests for headings, paragraphs, overlap, code fences, Serbian text, long unbroken content, empty content, and deterministic output.
- [ ] Build the canonical text prefix from title, summary, and tags.
- [ ] Calculate a deterministic SHA-256 content hash from all fields that influence chunks.
- [ ] Generate stable v2 record IDs and metadata.
- [ ] Make the collection name configurable, for example `KB_CHROMA_COLLECTION`, rather than adding another hard-coded production constant.
- [ ] Keep existing v1 embedding code operational until cutover.

### Incremental update semantics

For each SQLite entry:

1. Generate all chunks and the new content hash.
2. Upsert every new v2 chunk.
3. Verify that all expected IDs were accepted.
4. Delete older chunks for the same `entry_id` whose `content_hash` differs.
5. Mark the entry embedded only after the complete operation succeeds.

On failure, leave the entry eligible for retry. Never mark a partially written entry as successfully embedded.

The implementation must explicitly handle an entry shrinking from many chunks to fewer chunks so stale tail chunks do not survive.

## Phase 2 — build `kb_collection_v2` safely

The initial build must not touch `kb_collection` or depend on resetting current `embedded_at` values.

- [ ] Add a dedicated v2 reindex command/script that reads all SQLite entries independently of `embedded_at`.
- [ ] Create `kb_collection_v2` with cosine space.
- [ ] Process entries in small slices and fresh processes if needed; the previous all-at-once rebuild caused OOM kills.
- [ ] Keep embedding batches bounded. Short chunks reduce sequence memory, but the number of vectors will increase substantially.
- [ ] Record counts: SQLite entries, skipped empty entries, generated chunks, successful vectors, and failed entries.
- [ ] Verify persistence under `/data` and restart ChromaDB once before declaring the build valid.
- [ ] Run the v2 builder again to prove idempotency and stale-chunk cleanup.

Do not delete the old collection after this phase.

### Gemini records

The current collection also contains `gemini_*` records, while `query_chromadb()` explicitly skips them. Before migration:

- [ ] Identify every consumer of those records.
- [ ] Decide whether they belong in a separate collection.
- [ ] Do not copy them into `kb_collection_v2` by default if the KB Search API will continue ignoring them.

## Phase 3 — update retrieval and reranking in `kb-mcp`

Target repository: `/home/turok/projects/kb-mcp`

Primary file: `kb_search_api.py`

### API model changes

Extend `SearchResult` without immediately removing existing fields:

```python
matched_chunk: Optional[str] = None
chunk_index: Optional[int] = None
chunk_count: Optional[int] = None
```

Keep `content` temporarily for backward compatibility. Consumers should migrate to `matched_chunk`, after which returning full content can be reconsidered to save tokens.

### Chroma response handling

- [ ] Stop assuming that every Chroma ID can be converted directly to `int`.
- [ ] Read `entry_id`, `chunk_index`, `chunk_count`, and hash from metadata.
- [ ] Preserve the returned Chroma `document`; this is the matched chunk and reranker input.
- [ ] Enrich entry-level title, tags, summary, and date from SQLite using unique `entry_id` values.
- [ ] Treat missing/malformed metadata as an observable error or explicit compatibility path, not silent corruption.

### Cross-encoder changes

- [ ] Rerank `(query, prefixed_chunk_document)` pairs in a bounded batch.
- [ ] Do not slice the chunk again with `[:1500]`; the chunker owns the size contract.
- [ ] Capture raw score and sigmoid relevance during eval/debug runs.
- [ ] Group by `entry_id` after reranking and retain the highest-relevance chunk per entry.
- [ ] Keep at most one result per entry in the final response.
- [ ] Optionally attach an adjacent chunk later, but do not add that complexity before measuring single-chunk answers.

### Ordering and cutoff

Recommended order:

```text
chunk recall
  -> chunk rerank
  -> best chunk per entry
  -> relevance threshold
  -> entry-level time decay
  -> final-score sort
  -> output cap
```

Threshold inclusion must continue to use pre-decay relevance. Time decay is only an ordering correction.

Do not assume the existing `RERANK_THRESHOLD = 0.40` remains valid. Recalibrate it from the eval set because chunk-level inputs will change score distribution.

### Degraded mode

The current distance fallback and cross-encoder output are not calibrated to the same scale.

- [ ] Expose the active ranker mode in health/debug output (`cross-encoder` or `distance-fallback`).
- [ ] Define separate fallback cutoff behavior rather than applying the cross-encoder threshold blindly.
- [ ] Group fallback candidates by entry ID exactly like normal results.
- [ ] Consider a semaphore around CPU reranking to prevent concurrent threadpool requests from causing a CPU/RAM stampede.

## Phase 4 — update the Go consumer

Target file: `/home/turok/projects/kb-go/main.go`

- [ ] Add `MatchedChunk`, `ChunkIndex`, and `ChunkCount` JSON fields to `Result` and the Search API response struct.
- [ ] In `formatResults()`, prefer `MatchedChunk`; fall back to `Content` for v1 compatibility.
- [ ] Preserve title, source, date, tags, summary, and relevance band.
- [ ] Avoid printing the beginning of the full entry when a different chunk caused the match.
- [ ] Add Go tests proving `matched_chunk` is preferred and v1 responses still work.
- [ ] Reconsider the fixed `truncate(..., 3000)` only after measuring typical chunk size and LLM context usage.

## Phase 5 — side-by-side evaluation

Do not switch production based on anecdotal queries.

- [ ] Run every eval query against v1 and v2.
- [ ] Compare candidate recall before reranking.
- [ ] Compare final ranks and evidence chunks.
- [ ] Review all regressions manually.
- [ ] Record memory, vector count, collection size, p50 latency, and p95 latency.
- [ ] Test at least two simultaneous search requests.
- [ ] Test cross-encoder unavailable mode.
- [ ] Test Chroma restart and collection UUID refresh.
- [ ] Test an entry update that reduces chunk count.
- [ ] Test an entry deletion/orphan cleanup path.
- [ ] Test no-answer queries and confirm honest empty responses remain possible.

Suggested acceptance gates (final numbers must be agreed after baseline measurement):

- Expected-entry recall must not regress.
- Final top-5 quality should improve on long entries and remain neutral on short entries.
- False-empty rate must not increase.
- No-answer false positives must remain controlled.
- p95 latency must stay within an explicitly accepted interactive budget.
- No incomplete entry may be marked embedded.

## Phase 6 — production cutover

Make collection selection configuration-driven so rollback is one config change plus service restart.

Recommended sequence:

1. Confirm `kb_collection_v2` is complete and persistent.
2. Install the v2-aware `compile.py` but do not remove v1 rollback artifacts.
3. Run a final idempotent v2 sync to catch entries added during the initial build.
4. Deploy the v2-aware `kb_search_api.py` with collection set to `kb_collection_v2`.
5. Restart `kb-search-api`.
6. Verify `/health` reports the v2 collection and cross-encoder mode.
7. Run fixed smoke queries through `/kb/search`, `kb search`, `kb ask`, MCP `search`, and Open WebUI websearch format.
8. Confirm newly added entries are chunked automatically by `kb-watcher`.
9. Monitor errors, latency, empty-result rate, RAM, and CPU.

Do not delete `kb_collection` during cutover.

## Rollback

Rollback must not require rebuilding anything.

1. Point the Search API collection setting back to `kb_collection`.
2. Restore/deploy the v1-compatible Search API if the API contract itself caused the problem.
3. Restart `kb-search-api`.
4. Verify known smoke queries.
5. Keep v2 collection and logs for diagnosis; do not destroy evidence during rollback.

Retain the v1 collection for an agreed observation window, preferably at least seven days of normal use.

## Cleanup after successful observation

Only after the observation window and explicit approval:

- [ ] Remove v1 compatibility branches from Go and Python consumers.
- [ ] Stop returning full entry content if all consumers use `matched_chunk`.
- [ ] Update README architecture diagrams and rebuild instructions.
- [ ] Update `/home/turok/.gemini/SERVICES.md` if service contracts changed.
- [ ] Document final thresholds, vector counts, latency, and eval results in KB.
- [ ] Delete `kb_collection` only after a tested backup/rollback decision.
- [ ] Keep collection/schema version constants for future migrations.

## Files expected to change

### `kb-go`

- `compile.py`
- `main.go`
- `main_test.go`
- `README.md`
- new chunking module and tests
- new v2 reindex/eval tooling as needed

### `kb-mcp`

- `kb_search_api.py`
- `README.md`
- new API/reranking tests

### Live deployment

- `/opt/kb/compile.py`
- `/opt/kb/kb_search_api.py`
- `/usr/local/bin/kb`
- `/home/turok/.config/systemd/user/kb-search-api.service` or its environment file, only if collection selection is configured there

## Required verification commands

Use the actual repository commands discovered at implementation time. At minimum, the completed change must include:

```text
Go unit tests with FTS5 tag
Go race test
Go vet
Python syntax/tests
chunker determinism and boundary tests
v1 vs v2 eval report
live API health check
live smoke queries
Chroma persistence restart check
incremental add/update check
rollback rehearsal
```

Do not rely only on `curl` returning HTTP 200; inspect IDs, matched chunks, relevance, final ordering, and consumer output.

## Definition of done

The migration is complete only when all of the following are true:

- SQLite remains the single source of truth.
- Every non-empty entry is represented by one or more deterministic v2 chunks.
- Relevant text anywhere in an entry can participate in recall and reranking.
- The Search API returns the exact matched evidence chunk.
- Final results contain at most one result per SQLite entry.
- Thresholds are based on a saved eval set, not anecdotal tuning.
- Incremental add/update removes stale chunk versions.
- The watcher embeds new entries into v2 automatically.
- v1 remains immediately recoverable during the observation window.
- Tests, README, CHANGELOG, KB documentation, live deployment, and repository commits are all synchronized.
