#!/usr/bin/env python3
"""Provision and verify the two fixed KB storage profiles."""

import argparse
import json
import os
import pwd
import sqlite3
import urllib.request
from datetime import datetime, timezone
from pathlib import Path


SCHEMA = Path(__file__).with_name("schema.sql")
EMBEDDING_MODEL = "nomic-ai/nomic-embed-text-v1.5"
EMBEDDING_DIMENSION = 768
SCHEMA_VERSION = 1
CHROMA_HOST = "localhost"
CHROMA_PORT = 8000
RUNTIME_USER = "turok"

PROFILES = {
    "homelab": {
        "db": Path("/opt/kb/kb.db"),
        "collection": "kb_collection",
    },
    "ai": {
        "root": Path("/opt/ai-kb"),
        "db": Path("/opt/ai-kb/ai-kb.db"),
        "raw_dirs": (
            Path("/opt/ai-kb/raw"),
            Path("/opt/ai-kb/raw/notes"),
            Path("/opt/ai-kb/raw/urls"),
            Path("/opt/ai-kb/quarantine"),
        ),
        "collection": "ai_kb_collection",
    },
}


def expected_metadata(corpus):
    return {
        "hnsw:space": "cosine",
        "corpus": corpus,
        "embedding_model": EMBEDDING_MODEL,
        "embedding_dimension": EMBEDDING_DIMENSION,
        "schema_version": SCHEMA_VERSION,
    }


def merge_metadata(existing, corpus, now=None):
    existing = dict(existing or {})
    expected = expected_metadata(corpus)
    conflicts = {
        key: {"expected": value, "actual": existing[key]}
        for key, value in expected.items()
        if key in existing and existing[key] != value
    }
    if conflicts:
        raise RuntimeError(
            f"refusing metadata migration for {corpus}: "
            f"{json.dumps(conflicts, sort_keys=True)}"
        )
    merged = dict(existing)
    merged.update(expected)
    merged.setdefault("created_at", now or datetime.now(timezone.utc).isoformat())
    if not merged["created_at"]:
        raise RuntimeError(f"refusing empty created_at for {corpus}")
    return merged


def validate_metadata(metadata, corpus):
    merged = merge_metadata(metadata, corpus, now="validation-only")
    missing = [key for key in (*expected_metadata(corpus), "created_at") if key not in (metadata or {})]
    if missing:
        raise RuntimeError(f"{corpus} metadata missing required keys: {missing}")
    return merged


def validate_configuration(configuration, corpus):
    actual_space = ((configuration or {}).get("hnsw") or {}).get("space")
    if actual_space != "cosine":
        raise RuntimeError(
            f"{corpus} Chroma configuration must be cosine, got {actual_space!r}"
        )


def initialize_ai_sqlite():
    profile = PROFILES["ai"]
    profile["root"].mkdir(mode=0o700, parents=True, exist_ok=True)
    os.chmod(profile["root"], 0o700)
    for path in profile["raw_dirs"]:
        path.mkdir(mode=0o700, parents=True, exist_ok=True)
        os.chmod(path, 0o700)
    schema = SCHEMA.read_text(encoding="utf-8")
    db = sqlite3.connect(profile["db"])
    try:
        db.executescript(schema)
        db.commit()
    finally:
        db.close()
    os.chmod(profile["db"], 0o600)
    if os.geteuid() == 0:
        owner = pwd.getpwnam(RUNTIME_USER)
        for path in (profile["root"], *profile["raw_dirs"], profile["db"]):
            os.chown(path, owner.pw_uid, owner.pw_gid)


def sqlite_health(path):
    uri = f"file:{path}?mode=ro"
    db = sqlite3.connect(uri, uri=True)
    try:
        integrity = db.execute("PRAGMA integrity_check").fetchone()[0]
        objects = {row[0] for row in db.execute(
            "SELECT name FROM sqlite_master WHERE type IN ('table', 'trigger')"
        )}
        required = {
            "entries", "entries_fts", "entries_fts_insert",
            "entries_fts_update", "entries_fts_delete",
        }
        missing = sorted(required - objects)
        count = db.execute("SELECT COUNT(*) FROM entries").fetchone()[0]
        if integrity != "ok" or missing:
            raise RuntimeError(
                f"SQLite health failed for {path}: integrity={integrity} missing={missing}"
            )
        return {"integrity": integrity, "entries": count, "missing_objects": missing}
    finally:
        db.close()


def sqlite_id_sets(path):
    db = sqlite3.connect(f"file:{path}?mode=ro", uri=True)
    try:
        all_ids = {str(row[0]) for row in db.execute("SELECT id FROM entries")}
        embedded_ids = {
            str(row[0]) for row in db.execute(
                "SELECT id FROM entries WHERE embedded_at IS NOT NULL"
            )
        }
        return all_ids, embedded_ids
    finally:
        db.close()


def chroma_client():
    import chromadb
    return chromadb.HttpClient(host=CHROMA_HOST, port=CHROMA_PORT)


def replace_collection_metadata(collection, metadata):
    """Use Chroma's public v2 update endpoint for an atomic metadata replace.

    chromadb 1.5.x rejects any Collection.modify() payload containing the
    immutable legacy hnsw:space key, even when the value remains unchanged.
    The pinned 1.0.0 server accepts the same-value key through its documented
    update endpoint and preserves the actual HNSW configuration.
    """
    url = (
        f"http://{CHROMA_HOST}:{CHROMA_PORT}/api/v2/tenants/default_tenant/"
        f"databases/default_database/collections/{collection.id}"
    )
    payload = json.dumps({
        "new_metadata": metadata,
        "new_name": None,
        "new_configuration": None,
    }).encode("utf-8")
    request = urllib.request.Request(
        url,
        data=payload,
        headers={"Content-Type": "application/json"},
        method="PUT",
    )
    with urllib.request.urlopen(request, timeout=30) as response:
        if response.status != 200:
            raise RuntimeError(
                f"Chroma metadata update failed for {collection.name}: HTTP {response.status}"
            )


def provision_chroma(client):
    homelab = client.get_collection(PROFILES["homelab"]["collection"])
    homelab_metadata = merge_metadata(homelab.metadata, "homelab")

    names = {
        item if isinstance(item, str) else item.name
        for item in client.list_collections()
    }
    ai_name = PROFILES["ai"]["collection"]
    ai = client.get_collection(ai_name) if ai_name in names else None
    ai_metadata = merge_metadata(ai.metadata if ai else {}, "ai")

    # All conflicts are checked before the first mutation.
    replace_collection_metadata(homelab, homelab_metadata)
    if ai is None:
        ai = client.create_collection(name=ai_name, metadata=ai_metadata)
    else:
        replace_collection_metadata(ai, ai_metadata)

    for corpus, name in (("homelab", PROFILES["homelab"]["collection"]), ("ai", ai_name)):
        collection = client.get_collection(name)
        validate_metadata(collection.metadata, corpus)
        validate_configuration(collection.configuration_json, corpus)


def prepare_ai_index_rebuild(client):
    """Fail-closed preparation for rebuilding a missing or incomplete AI index.

    Existing vectors are never deleted. SQLite rows whose embedded marker has no
    corresponding Chroma ID are reset to pending; compile.py then re-embeds them.
    Chroma IDs with no SQLite source abort the operation for manual inspection.
    """
    profile = PROFILES["ai"]
    sqlite_health(profile["db"])
    all_ids, embedded_ids = sqlite_id_sets(profile["db"])
    names = {
        item if isinstance(item, str) else item.name
        for item in client.list_collections()
    }
    collection_name = profile["collection"]
    collection = client.get_collection(collection_name) if collection_name in names else None
    actual_ids = set()
    if collection is not None:
        validate_metadata(collection.metadata, "ai")
        validate_configuration(collection.configuration_json, "ai")
        actual_ids = set(collection.get(include=[])["ids"])

    orphan_ids = sorted(actual_ids - all_ids)
    if orphan_ids:
        raise RuntimeError(
            f"refusing AI index repair with Chroma orphans: {orphan_ids[:20]}"
        )
    missing_ids = sorted(embedded_ids - actual_ids, key=int)
    if missing_ids:
        db = sqlite3.connect(profile["db"])
        try:
            placeholders = ",".join("?" for _ in missing_ids)
            db.execute(
                f"UPDATE entries SET embedded_at=NULL WHERE id IN ({placeholders})",
                [int(value) for value in missing_ids],
            )
            db.commit()
        finally:
            db.close()

    created = False
    if collection is None:
        collection = client.create_collection(
            name=collection_name,
            metadata=merge_metadata({}, "ai"),
        )
        created = True
    validate_metadata(collection.metadata, "ai")
    validate_configuration(collection.configuration_json, "ai")
    return {
        "collection_created": created,
        "reset_to_pending": len(missing_ids),
        "pending_ids": missing_ids,
    }


def health_report(client=None):
    client = client or chroma_client()
    report = {"sqlite": {}, "chroma": {}}
    for corpus, profile in PROFILES.items():
        report["sqlite"][corpus] = sqlite_health(profile["db"])
        all_ids, embedded_ids = sqlite_id_sets(profile["db"])
        collection = client.get_collection(profile["collection"])
        validate_metadata(collection.metadata, corpus)
        validate_configuration(collection.configuration_json, corpus)
        chroma_ids = set(collection.get(include=[])["ids"])
        missing_vectors = sorted(embedded_ids - chroma_ids)
        orphan_vectors = sorted(chroma_ids - all_ids)
        if missing_vectors or orphan_vectors:
            raise RuntimeError(
                f"{corpus} SQLite/Chroma ID mismatch: "
                f"missing={missing_vectors[:20]} orphans={orphan_vectors[:20]}"
            )
        report["chroma"][corpus] = {
            "collection": profile["collection"],
            "count": collection.count(),
            "metadata": collection.metadata,
            "missing_vectors": missing_vectors,
            "orphan_vectors": orphan_vectors,
        }
    return report


def parse_args(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    mode = parser.add_mutually_exclusive_group(required=True)
    mode.add_argument("--apply", action="store_true")
    mode.add_argument("--health", action="store_true")
    mode.add_argument(
        "--prepare-ai-index-rebuild",
        action="store_true",
        help="reset only missing AI vectors to pending; never deletes vectors",
    )
    return parser.parse_args(argv)


def main(argv=None):
    args = parse_args(argv)
    client = chroma_client()
    if args.prepare_ai_index_rebuild:
        print(json.dumps(prepare_ai_index_rebuild(client), sort_keys=True))
        return
    if args.apply:
        initialize_ai_sqlite()
        provision_chroma(client)
    print(json.dumps(health_report(client), ensure_ascii=False, sort_keys=True))


if __name__ == "__main__":
    main()
