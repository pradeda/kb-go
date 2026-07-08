#!/usr/bin/env python3
# /opt/kb/compile.py

import sqlite3, os, re, sys, json
from datetime import datetime
from pathlib import Path

# --- configuration ---
KB          = Path("/opt/kb")
DB          = KB / "kb.db"
WIKI        = KB / "wiki"
RAW         = KB / "raw"
PROMPT_FILE = KB / "prompts" / "compiler.md"
ENV_FILE    = KB / ".env"

OPENROUTER_MODEL  = "google/gemini-2.0-flash-lite-001"
EMBED_MODEL       = "nomic-ai/nomic-embed-text-v1.5"
CHROMA_HOST       = "localhost"
CHROMA_PORT       = 8000
CHROMA_COLLECTION = "kb_collection"
BATCH_SIZE        = 5
MAX_TOKENS        = 16000

# --- load .env ---
if ENV_FILE.exists():
    for line in ENV_FILE.read_text().splitlines():
        line = line.strip()
        if line and not line.startswith("#") and "=" in line:
            k, v = line.split("=", 1)
            os.environ.setdefault(k.strip(), v.strip())

API_KEY = os.environ.get("OPENROUTER_API_KEY")
if not API_KEY:
    print("ERROR: OPENROUTER_API_KEY not set in /opt/kb/.env")
    sys.exit(1)

def get_db():
    return sqlite3.connect(str(DB))

def read_file_safe(path):
    try:
        return Path(path).read_text(encoding="utf-8")
    except Exception:
        return ""

def get_uncompiled():
    db = get_db()
    rows = db.execute("""
        SELECT id, type, content, title, tags, raw_path, created_at
        FROM entries WHERE compiled_at IS NULL ORDER BY created_at
    """).fetchall()
    db.close()
    return rows

def get_unembedded():
    db = get_db()
    rows = db.execute("""
        SELECT id, type, content, title, tags, summary, raw_path
        FROM entries WHERE embedded_at IS NULL ORDER BY created_at
    """).fetchall()
    db.close()
    return rows

def mark_compiled(ids):
    db = get_db()
    ph = ",".join("?" * len(ids))
    db.execute(f"UPDATE entries SET compiled_at=? WHERE id IN ({ph})",
               [datetime.now().isoformat()] + list(ids))
    db.commit()
    db.close()

def mark_embedded(ids):
    db = get_db()
    ph = ",".join("?" * len(ids))
    db.execute(f"UPDATE entries SET embedded_at=? WHERE id IN ({ph})",
               [datetime.now().isoformat()] + list(ids))
    db.commit()
    db.close()

def update_metadata_from_file(raw_path):
    wiki_path = KB / "wiki" / "sources" / Path(raw_path).name
    if not wiki_path.exists():
        return

    content = wiki_path.read_text(encoding="utf-8")
    tags_match = re.search(r"^tags:\s*(.*)$", content, re.MULTILINE)
    tags = tags_match.group(1).strip() if tags_match else ""
    
    summary = ""
    parts = content.split("---")
    if len(parts) >= 3:
        body = parts[2].strip()
        lines = [l for l in body.split("\n") if l.strip() and not l.strip().startswith("#")]
        if lines:
            summary = lines[0][:250].strip()

    db = get_db()
    try:
        db.execute("UPDATE entries SET tags=?, summary=? WHERE raw_path=?", (tags, summary, str(raw_path)))
        db.commit()
    finally:
        db.close()

def call_openrouter(system_prompt, user_message):
    import urllib.request, time
    encoded = json.dumps({
        "model": OPENROUTER_MODEL,
        "max_tokens": MAX_TOKENS,
        "response_format": {"type": "json_object"},
        "messages": [
            {"role": "system", "content": system_prompt},
            {"role": "user",   "content": user_message},
        ],
    }).encode()
    last_error = None
    for attempt in range(3):
        try:
            req = urllib.request.Request(
                "https://openrouter.ai/api/v1/chat/completions",
                data=encoded,
                headers={"Authorization": f"Bearer {API_KEY}", "Content-Type": "application/json"},
                method="POST",
            )
            with urllib.request.urlopen(req, timeout=120) as resp:
                data = json.loads(resp.read())
            if "error" in data:
                raise Exception(str(data["error"]))
            return data["choices"][0]["message"]["content"]
        except urllib.error.HTTPError as e:
            last_error = f"HTTP {e.code}"
            if e.code == 429:
                wait = 10 * (2 ** attempt)
                print(f"  [RATE LIMIT] Waiting {wait}s (attempt {attempt+1}/3)...")
                time.sleep(wait)
            elif e.code >= 500:
                time.sleep(5 * (attempt + 1))
            else:
                raise
        except Exception as e:
            last_error = str(e)
            if attempt < 2:
                time.sleep(3)
    raise Exception(f"OpenRouter failed after 3 attempts: {last_error}")

def get_chroma_collection():
    import chromadb
    client = chromadb.HttpClient(host=CHROMA_HOST, port=CHROMA_PORT)
    return client.get_or_create_collection(
        name=CHROMA_COLLECTION,
        metadata={"hnsw:space": "cosine"}
    )

_embed_model = None

def get_embed_model():
    global _embed_model
    if _embed_model is None:
        from fastembed import TextEmbedding
        _embed_model = TextEmbedding(EMBED_MODEL)
    return _embed_model

EMBED_BATCH_SIZE = 50

def embed_entries(entries):
    print(f"\nEmbedding {len(entries)} entries to ChromaDB (FastEmbed batch)...")
    collection = get_chroma_collection()
    model = get_embed_model()
    has_error = False
    successful_ids = []

    for chunk_start in range(0, len(entries), EMBED_BATCH_SIZE):
        chunk = entries[chunk_start:chunk_start + EMBED_BATCH_SIZE]
        ids, texts, metadatas, filtered_rows = [], [], [], []

        for row in chunk:
            entry_id, etype, content, title, tags, summary, raw_path = row
            text_parts = []
            if title:   text_parts.append(f"Title: {title}")
            if summary: text_parts.append(f"Summary: {summary}")
            if content: text_parts.append(content[:8000])
            if tags:    text_parts.append(f"Tags: {tags}")
            if not text_parts:
                print(f"  [SKIP] #{entry_id}: empty content")
                continue
            ids.append(str(entry_id))
            texts.append("\n".join(text_parts))
            filtered_rows.append(row)
            metadatas.append({
                "type": etype,
                "title": title or "",
                "tags": tags or "",
                "raw_path": raw_path or "",
            })

        if not ids:
            continue

        try:
            embeddings = [e.tolist() for e in model.passage_embed(texts)]
            collection.upsert(
                ids=ids,
                embeddings=embeddings,
                documents=texts,
                metadatas=metadatas,
            )
            for entry_id, row in zip(ids, filtered_rows):
                label = row[3] or row[2][:40]
                print(f"  [OK] embed #{entry_id}: {label}")
            successful_ids.extend(ids)
        except Exception as e:
            print(f"  [ERROR] batch {chunk_start}-{chunk_start + len(chunk)}: {e}")
            has_error = True

    return successful_ids

def parse_and_write(response_text):
    written = []

    # JSON mode: {"files": [{"path": "...", "content": "..."}]}
    try:
        data = json.loads(response_text)
        files = data.get("files", [])
        if not isinstance(files, list):
            raise ValueError(f"Expected list, got {type(files)}")
        for f in files:
            rel_path = f.get("path", "").strip()
            content = f.get("content", "").strip()
            if not rel_path or rel_path == "wiki/index.md":
                continue
            full_path = (KB / rel_path).resolve()
            if not full_path.is_relative_to(KB.resolve()):
                print(f"  [SKIP] path escape: {rel_path}")
                continue
            full_path.parent.mkdir(parents=True, exist_ok=True)
            full_path.write_text(content, encoding="utf-8")
            written.append(rel_path)
            print(f"  [OK] {rel_path}")
        return written
    except (json.JSONDecodeError, AttributeError, ValueError):
        pass

    # Fallback: legacy FILE:/=== format
    blocks = re.split(r"(?=^FILE:\s)", response_text, flags=re.MULTILINE)
    for block in blocks:
        m = re.match(r"FILE:\s*(.+?)\n===\n(.*?)\n===", block, re.DOTALL)
        if not m:
            continue
        rel_path = m.group(1).strip()
        content = m.group(2).strip()
        if rel_path == "wiki/index.md":
            continue
        full_path = (KB / rel_path).resolve()
        if not full_path.is_relative_to(KB.resolve()):
            print(f"  [SKIP] path escape: {rel_path}")
            continue
        full_path.parent.mkdir(parents=True, exist_ok=True)
        full_path.write_text(content, encoding="utf-8")
        written.append(rel_path)
        print(f"  [OK] {rel_path}")
    return written

def _extract_concept_desc(file_content):
    in_def = False
    for line in file_content.split("\n"):
        if line.strip() == "## Definition":
            in_def = True
            continue
        if in_def:
            if line.startswith("## "):
                break
            stripped = line.strip()
            if stripped:
                return stripped[:150]
    # Fallback: skip entire frontmatter block (--- to ---), take first meaningful line
    dash_count = 0
    for line in file_content.split("\n"):
        stripped = line.strip()
        if stripped == "---" and dash_count < 2:
            dash_count += 1
            continue
        if dash_count < 2:
            continue
        if stripped and not stripped.startswith("#"):
            return stripped[:150]
    return ""

def _insert_into_section(content, section_header, new_line):
    lines = content.split("\n")
    in_section = False
    insert_at = len(lines)
    for i, line in enumerate(lines):
        if line.strip() == section_header:
            in_section = True
            continue
        if in_section and line.startswith("## "):
            insert_at = i
            break
    lines.insert(insert_at, new_line)
    return "\n".join(lines)

def update_index(written_paths):
    index_path = WIKI / "index.md"
    content = index_path.read_text(encoding="utf-8")
    changed = False

    for rel_path in written_paths:
        full_path = KB / rel_path
        if not full_path.exists():
            continue
        file_content = full_path.read_text(encoding="utf-8")
        slug = Path(rel_path).stem

        if rel_path.startswith("wiki/sources/"):
            link = f"[[sources/{slug}]]"
            if link in content:
                continue
            title_m = re.search(r"^title:\s*(.+)$", file_content, re.MULTILINE)
            saved_m = re.search(r"^saved:\s*(\d{4}-\d{2}-\d{2})", file_content, re.MULTILINE)
            title = title_m.group(1).strip() if title_m else slug
            date = saved_m.group(1) if saved_m else ""
            content = _insert_into_section(content, "## Sources", f"- {link} — {title}, {date}")
            changed = True

        elif rel_path.startswith("wiki/concepts/"):
            link = f"[[concepts/{slug}]]"
            if link in content:
                continue
            desc = _extract_concept_desc(file_content)
            content = _insert_into_section(content, "## Concepts", f"- {link} — {desc}")
            changed = True

    if changed:
        today = datetime.now().strftime("%Y-%m-%d")
        content = re.sub(r"^(Last modified|Poslednja izmena):.*$", f"Last modified: {today}", content, flags=re.MULTILINE)
        index_path.write_text(content, encoding="utf-8")
        print(f"  [OK] wiki/index.md (programmatic)")

def compile_entries(entries):
    print(f"\nCompiling {len(entries)} entries...")
    system_prompt = PROMPT_FILE.read_text(encoding="utf-8")
    all_compiled = []

    for i in range(0, len(entries), BATCH_SIZE):
        batch = entries[i:i + BATCH_SIZE]
        print(f"\n  Batch {i//BATCH_SIZE + 1}...")

        try:
            existing_concepts = sorted(p.stem for p in (WIKI / "concepts").glob("*.md"))
            user_msg = (
                "NOTE: Do not generate FILE: wiki/index.md — it is updated automatically.\n"
                f"Existing concepts: {', '.join(existing_concepts)}\n\n"
                "Entries:\n" +
                "".join([f"=== {r[5]} ===\n{read_file_safe(r[5]) if r[5] else r[2]}" for r in batch])
            )
            response = call_openrouter(system_prompt, user_msg)
            written = parse_and_write(response)
            if written:
                all_compiled += [row[0] for row in batch]
                for row in batch:
                    if row[5]: update_metadata_from_file(row[5])
                update_index(written)
        except Exception as e:
            import traceback
            print(f"  ERROR: {e}")
            traceback.print_exc()
            continue
    return all_compiled

def recover_db_from_raw():
    """Parse all raw .md files and rebuild SQLite database."""
    print("Recover DB: parsing raw .md files...")
    recovered = 0
    skipped = 0

    db = get_db()
    db.execute("BEGIN TRANSACTION")
    try:
        for subdir in ["notes", "urls"]:
            raw_sub = RAW / subdir
            if not raw_sub.exists():
                continue
            for md_file in sorted(raw_sub.glob("*.md")):
                try:
                    content = md_file.read_text(encoding="utf-8")
                except UnicodeDecodeError:
                    content = md_file.read_text(encoding="latin-1")
                # Parse frontmatter
                fm = {}
                if content.startswith("---"):
                    parts = content.split("---", 2)
                    if len(parts) >= 3:
                        for line in parts[1].strip().split("\n"):
                            if ":" in line:
                                k, v = line.split(":", 1)
                                fm[k.strip()] = v.strip()
                        body = parts[2].strip()
                    else:
                        body = content
                else:
                    body = content

                etype = fm.get("type", subdir.rstrip("s"))  # "notes"→"note", "urls"→"url"
                if etype not in ("note", "url"):
                    etype = "note"  # default
                title = fm.get("title", md_file.stem)
                tags = fm.get("tags", "")
                saved = fm.get("saved", datetime.fromtimestamp(md_file.stat().st_mtime).strftime("%Y-%m-%dT%H:%M:%S"))

                row = db.execute("SELECT id FROM entries WHERE raw_path = ?", (str(md_file),)).fetchone()
                if row:
                    skipped += 1
                    continue

                db.execute(
                    "INSERT INTO entries (type, content, title, tags, raw_path, source, created_at) "
                    "VALUES (?, ?, ?, ?, ?, 'recovery', ?)",
                    (etype, body, title, tags, str(md_file), saved),
                )
                recovered += 1
                print(f"  [OK] {md_file.name}")

        db.commit()
    except Exception as e:
        db.rollback()
        print(f"Error during DB recovery: {e}")
        raise
    finally:
        db.close()

    print(f"\nRecover DB done: {recovered} recovered, {skipped} skipped (already in DB)")

def recover_raw_from_db():
    """Rebuild raw .md files from SQLite database."""
    print("Recover RAW: generating .md files from SQLite...")
    recovered = 0
    skipped = 0

    db = get_db()
    rows = db.execute(
        "SELECT id, type, content, title, tags, created_at, raw_path FROM entries ORDER BY created_at"
    ).fetchall()
    db.close()

    for row in rows:
        eid, etype, content, title, tags, created, raw_path = row
        raw_file = Path(raw_path) if raw_path else None

        # Determine path
        subdir = "notes" if etype == "note" else "urls"
        date_str = created[:10] if created else "unknown"
        slug = re.sub(r"[^a-z0-9-]", "", title.lower().replace(" ", "-"))[:60] if title else f"entry-{eid}"

        use_existing = False
        if raw_file:
            try:
                use_existing = raw_file.exists()
            except OSError:
                pass  # path too long, regenerate

        if use_existing:
            skipped += 1
            continue

        if not raw_file or not use_existing:
            raw_file = RAW / subdir / f"{date_str}-{slug}.md"
        raw_file.parent.mkdir(parents=True, exist_ok=True)

        fm = f"---\ntype: {etype}\ntitle: {title}\ntags: {tags}\nsaved: {created}\n---\n\n{content}"
        raw_file.write_text(fm, encoding="utf-8")
        recovered += 1
        print(f"  [OK] {raw_file.name}")

    print(f"\nRecover RAW done: {recovered} recovered, {skipped} skipped (already exists)")

def main():
    if "--recover-db" in sys.argv:
        recover_db_from_raw()
        return
    if "--recover-raw" in sys.argv:
        recover_raw_from_db()
        return

    # Wiki generation disabled — not in use. Kept for future reactivation.
    # uncompiled = get_uncompiled()
    # if uncompiled:
    #     compiled_ids = compile_entries(uncompiled)
    #     if compiled_ids: mark_compiled(compiled_ids)

    unembedded = get_unembedded()
    if unembedded:
        embedded_ids = embed_entries(unembedded)
        if embedded_ids:
            mark_embedded(embedded_ids)

if __name__ == "__main__":
    main()
