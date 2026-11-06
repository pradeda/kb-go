import importlib.util
import contextlib
import io
import json
import pathlib
import sqlite3
import subprocess
import tempfile
import unittest
from unittest import mock
from datetime import datetime


MODULE_PATH = pathlib.Path(__file__).resolve().parent.parent / "runtime" / "compile.py"
SPEC = importlib.util.spec_from_file_location("kb_compile", MODULE_PATH)
kb_compile = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(kb_compile)


class CompileRunStampTests(unittest.TestCase):
    """last_compile answers 'is the compiler still running', not 'when was the last write'."""

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.db_path = pathlib.Path(self.temp.name) / "corpus.db"
        self._saved_db = kb_compile.DB
        kb_compile.DB = self.db_path
        db = sqlite3.connect(self.db_path)
        db.execute("CREATE TABLE entries(id INTEGER PRIMARY KEY, embedded_at TEXT)")
        db.commit()
        db.close()

    def tearDown(self):
        kb_compile.DB = self._saved_db
        self.temp.cleanup()

    def _stored(self):
        db = sqlite3.connect(self.db_path)
        try:
            row = db.execute("SELECT value FROM kb_meta WHERE key='last_compile'").fetchone()
        finally:
            db.close()
        return row[0] if row else None

    def test_creates_the_table_on_first_run(self):
        stamp = kb_compile.record_compile_run()
        self.assertIsNotNone(stamp)
        self.assertEqual(self._stored(), stamp)

    def test_second_run_overwrites_rather_than_duplicating(self):
        first = kb_compile.record_compile_run()
        second = kb_compile.record_compile_run()
        self.assertNotEqual(first, second)
        db = sqlite3.connect(self.db_path)
        try:
            count = db.execute("SELECT COUNT(*) FROM kb_meta").fetchone()[0]
        finally:
            db.close()
        self.assertEqual(count, 1)
        self.assertEqual(self._stored(), second)

    def test_timestamp_carries_an_explicit_offset(self):
        # Read back by /v2/health and compared against now, so a naive local
        # string would be ambiguous across DST.
        stamp = kb_compile.record_compile_run()
        self.assertIsNotNone(datetime.fromisoformat(stamp).tzinfo)

    def test_unwritable_database_warns_instead_of_failing_the_run(self):
        kb_compile.DB = pathlib.Path("/nonexistent/dir/corpus.db")
        buffer = io.StringIO()
        with contextlib.redirect_stdout(buffer):
            self.assertIsNone(kb_compile.record_compile_run())
        self.assertIn("last_compile not recorded", buffer.getvalue())


class CorpusProfileTests(unittest.TestCase):
    def test_default_profile_is_legacy_homelab(self):
        args = kb_compile.parse_args([])
        self.assertEqual(args.corpus, "homelab")
        profile = kb_compile.configure_corpus(args.corpus)
        self.assertEqual(profile["db"], "/opt/kb/kb.db")
        self.assertEqual(profile["collection"], "kb_collection")

    def test_ai_profile_is_physically_separate(self):
        homelab = kb_compile.CORPUS_PROFILES["homelab"]
        ai = kb_compile.CORPUS_PROFILES["ai"]
        for key in (
            "root", "db", "raw", "env", "collection", "secret_patterns",
            "quarantine_dir", "quarantine_log", "watcher_lock", "watcher_state",
        ):
            self.assertNotEqual(homelab[key], ai[key], key)
        self.assertEqual(ai["wiki_index"], "")

    def test_cli_accepts_only_allowlisted_corpora(self):
        self.assertEqual(kb_compile.parse_args(["--corpus", "ai"]).corpus, "ai")
        with contextlib.redirect_stderr(io.StringIO()):
            with self.assertRaises(SystemExit):
                kb_compile.parse_args(["--corpus", "/tmp/arbitrary"])

    def test_metadata_validation_is_fail_closed(self):
        kb_compile.configure_corpus("ai")
        valid = {
            "hnsw:space": "cosine",
            "corpus": "ai",
            "embedding_model": kb_compile.EMBED_MODEL,
            "embedding_dimension": 768,
            "schema_version": 1,
            "created_at": "2026-08-02T00:00:00+00:00",
        }
        kb_compile.validate_collection_metadata(valid)
        invalid = dict(valid, corpus="homelab")
        with self.assertRaises(RuntimeError):
            kb_compile.validate_collection_metadata(invalid)
        kb_compile.validate_collection_configuration({"hnsw": {"space": "cosine"}})
        with self.assertRaises(RuntimeError):
            kb_compile.validate_collection_configuration({"hnsw": {"space": "l2"}})

    def test_watchers_have_separate_allowlisted_state(self):
        script = str(MODULE_PATH.with_name("watcher.sh"))
        outputs = {}
        for corpus in ("homelab", "ai"):
            result = subprocess.run(
                ["bash", script, "--corpus", corpus, "--show-profile"],
                check=True,
                text=True,
                capture_output=True,
            )
            outputs[corpus] = dict(
                line.split("=", 1) for line in result.stdout.strip().splitlines()
            )
        self.assertNotEqual(outputs["homelab"]["raw"], outputs["ai"]["raw"])
        self.assertNotEqual(outputs["homelab"]["lock"], outputs["ai"]["lock"])
        self.assertNotEqual(outputs["homelab"]["state"], outputs["ai"]["state"])
        self.assertTrue(
            outputs["homelab"]["compile"].startswith(
                "/opt/kb/venv-embed/bin/python /opt/kb/compile.py"
            )
        )
        self.assertIn("--corpus ai", outputs["ai"]["compile"])

    def _retire_fixture(self, tmp):
        """Minimalni corpus: entries + FTS5 + trigeri, jedan unos i raw fajl."""
        import sqlite3

        db_path = pathlib.Path(tmp) / "kb.db"
        raw_path = pathlib.Path(tmp) / "note.md"
        raw_path.write_text("# Note\n\nbody\n", encoding="utf-8")
        con = sqlite3.connect(db_path)
        con.executescript(
            """
            CREATE TABLE entries (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                type TEXT NOT NULL CHECK(type IN ('url','note')),
                content TEXT NOT NULL, title TEXT DEFAULT '',
                summary TEXT DEFAULT '', tags TEXT DEFAULT '',
                raw_path TEXT DEFAULT '', source TEXT DEFAULT 'telegram',
                compiled_at DATETIME, embedded_at DATETIME,
                created_at DATETIME DEFAULT CURRENT_TIMESTAMP);
            CREATE VIRTUAL TABLE entries_fts USING fts5(title, content, summary, tags);
            CREATE TRIGGER entries_fts_delete AFTER DELETE ON entries BEGIN
              DELETE FROM entries_fts WHERE rowid=old.id;
            END;
            CREATE TRIGGER entries_fts_update AFTER UPDATE ON entries BEGIN
              DELETE FROM entries_fts WHERE rowid=old.id;
              INSERT INTO entries_fts(rowid,title,content,summary,tags)
              VALUES(new.id,new.title,new.content,new.summary,new.tags);
            END;
            """
        )
        con.execute(
            "INSERT INTO entries(id,type,content,title,raw_path,embedded_at) "
            "VALUES(7,'note','body','Note',?,CURRENT_TIMESTAMP)",
            (str(raw_path),),
        )
        con.execute(
            "INSERT INTO entries_fts(rowid,title,content,summary,tags) "
            "VALUES(7,'Note','body','','')"
        )
        con.commit()
        con.close()
        return db_path, raw_path

    def test_supersede_preserves_id_and_marks_every_source_layer(self):
        import sqlite3

        with tempfile.TemporaryDirectory() as tmp:
            db_path, raw_path = self._retire_fixture(tmp)
            report = kb_compile.supersede_entry(
                7, "KB 626 and KB 627", db_path=db_path
            )
            self.assertFalse(report["already_superseded"])
            con = sqlite3.connect(db_path)
            row = con.execute(
                "SELECT title,content,embedded_at FROM entries WHERE id=7"
            ).fetchone()
            self.assertTrue(row[0].startswith("[SUPERSEDED]"))
            self.assertTrue(row[1].startswith("SUPERSEDED — use KB 626 and KB 627"))
            self.assertIsNone(row[2])
            self.assertEqual(
                con.execute(
                    "SELECT COUNT(*) FROM entries_fts WHERE entries_fts MATCH 'SUPERSEDED'"
                ).fetchone()[0],
                1,
            )
            con.close()
            raw = raw_path.read_text(encoding="utf-8")
            self.assertIn('title: "[SUPERSEDED] Note"', raw)
            self.assertIn("SUPERSEDED — use KB 626 and KB 627", raw)

    def test_retire_removes_entry_from_every_layer(self):
        class FakeCollection:
            def __init__(self):
                self.ids = {"7"}

            def delete(self, ids):
                self.ids -= set(ids)

            def get(self, ids=None, include=None):
                present = sorted(self.ids & set(ids)) if ids else sorted(self.ids)
                return {"ids": present}

        import sqlite3

        with tempfile.TemporaryDirectory() as tmp:
            db_path, raw_path = self._retire_fixture(tmp)
            collection = FakeCollection()
            report = kb_compile.retire_entry(7, collection=collection, db_path=db_path)
            self.assertEqual(report["entry_id"], 7)
            self.assertEqual(collection.ids, set())
            self.assertFalse(raw_path.exists())
            con = sqlite3.connect(db_path)
            self.assertEqual(con.execute("SELECT COUNT(*) FROM entries WHERE id=7").fetchone()[0], 0)
            self.assertEqual(con.execute("SELECT COUNT(*) FROM entries_fts").fetchone()[0], 0)
            con.close()

    def test_retire_is_fail_closed_when_vector_survives(self):
        """Ako Chroma ne obrise vektor, SQLite red mora ostati netaknut."""
        class StubbornCollection:
            def delete(self, ids):
                return None

            def get(self, ids=None, include=None):
                return {"ids": list(ids or [])}

        import sqlite3

        with tempfile.TemporaryDirectory() as tmp:
            db_path, raw_path = self._retire_fixture(tmp)
            with self.assertRaises(RuntimeError):
                kb_compile.retire_entry(7, collection=StubbornCollection(), db_path=db_path)
            con = sqlite3.connect(db_path)
            self.assertEqual(con.execute("SELECT COUNT(*) FROM entries WHERE id=7").fetchone()[0], 1)
            con.close()
            self.assertTrue(raw_path.exists())

    def test_retire_rejects_unknown_entry(self):
        class FakeCollection:
            def delete(self, ids):
                return None

            def get(self, ids=None, include=None):
                return {"ids": []}

        with tempfile.TemporaryDirectory() as tmp:
            db_path, _ = self._retire_fixture(tmp)
            with self.assertRaises(LookupError):
                kb_compile.retire_entry(999, collection=FakeCollection(), db_path=db_path)

    def test_retire_is_exclusive_with_other_recovery_modes(self):
        self.assertEqual(kb_compile.parse_args(["--retire", "42"]).retire, 42)
        with contextlib.redirect_stderr(io.StringIO()):
            with self.assertRaises(SystemExit):
                kb_compile.parse_args(["--retire", "42", "--health"])
            with self.assertRaises(SystemExit):
                kb_compile.parse_args(["--retire", "not-a-number"])

    def test_secret_rules_are_fail_closed(self):
        original = kb_compile.SECRET_PATTERNS_FILE
        try:
            kb_compile.SECRET_PATTERNS_FILE = pathlib.Path(tempfile.gettempdir()) / "missing-kb-secret-rules.json"
            kb_compile._secret_rules = None
            with self.assertRaises(RuntimeError):
                kb_compile.load_secret_rules()
        finally:
            kb_compile.SECRET_PATTERNS_FILE = original
            kb_compile._secret_rules = None

    def test_secret_rules_reject_semantically_empty_config(self):
        original = kb_compile.SECRET_PATTERNS_FILE
        try:
            with tempfile.TemporaryDirectory() as tmp:
                for index, body in enumerate(("{}", '{"version":1,"patterns":[]}')):
                    path = pathlib.Path(tmp) / f"rules-{index}.json"
                    path.write_text(body, encoding="utf-8")
                    kb_compile.SECRET_PATTERNS_FILE = path
                    kb_compile._secret_rules = None
                    with self.assertRaises(RuntimeError):
                        kb_compile.load_secret_rules()
        finally:
            kb_compile.SECRET_PATTERNS_FILE = original
            kb_compile._secret_rules = None


SECRET_TITLE = "POSTGRES_PASSWORD=hunter2xyz"


class Gate2QuarantineTests(unittest.TestCase):
    """Gate 2 must quarantine every original field it redacts, not only the body.

    A secret that appeared only in the title used to leave nothing behind: the row
    was redacted (the title-hit accounting already worked here) but the .orig backup
    held the clean body, so the original title was gone. Go Gate 1 now records the
    same payload for the fields it has (title, content) - see quarantinePayload in
    secretscan.go.
    """

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        root = pathlib.Path(self.temp.name)
        self.db_path = root / "corpus.db"
        self.raw_path = root / "raw" / "notes" / "2026-09-25-note.md"
        self.raw_path.parent.mkdir(parents=True)
        self.raw_path.write_text("prefix header\nPOSTGRES_PASSWORD=hunter2xyz\nsuffix footer", encoding="utf-8")
        db = sqlite3.connect(self.db_path)
        db.executescript(
            (MODULE_PATH.parent.parent / "config" / "schema.sql").read_text(encoding="utf-8")
        )
        db.execute(
            "INSERT INTO entries (id, type, content, title, tags, summary, raw_path)"
            " VALUES (1, 'note', 'clean body', ?, 't', '', ?)",
            (SECRET_TITLE, str(self.raw_path)),
        )
        db.commit()
        db.close()
        self._saved = (
            kb_compile.DB, kb_compile.QUARANTINE_DIR, kb_compile.QUARANTINE_LOG,
            kb_compile.SECRET_PATTERNS_FILE, kb_compile._secret_rules,
        )
        kb_compile.DB = self.db_path
        kb_compile.QUARANTINE_DIR = root / "quarantine"
        kb_compile.QUARANTINE_LOG = root / "quarantine.log"
        kb_compile.SECRET_PATTERNS_FILE = MODULE_PATH.parent.parent / "config" / "secret_patterns.json"
        kb_compile._secret_rules = None

    def tearDown(self):
        (
            kb_compile.DB, kb_compile.QUARANTINE_DIR, kb_compile.QUARANTINE_LOG,
            kb_compile.SECRET_PATTERNS_FILE, kb_compile._secret_rules,
        ) = self._saved
        self.temp.cleanup()

    def test_title_only_secret_is_quarantined_with_its_original_title(self):
        rows = kb_compile.get_unembedded()
        self.assertEqual(len(rows), 1)
        with contextlib.redirect_stdout(io.StringIO()):
            kb_compile.sanitize_unembedded(rows)

        db = sqlite3.connect(self.db_path)
        try:
            title, content = db.execute("SELECT title, content FROM entries WHERE id=1").fetchone()
        finally:
            db.close()
        self.assertNotIn("hunter2xyz", title, "title was not redacted")
        self.assertIn("<REDACTED_SECRET>", title)
        self.assertEqual(content, "clean body")

        # In-place raw redaction preserved with surrounding text kept
        raw_text = self.raw_path.read_text(encoding="utf-8")
        self.assertNotIn("hunter2xyz", raw_text)
        self.assertIn("<REDACTED_SECRET>", raw_text)
        self.assertIn("prefix header\nPOSTGRES_PASSWORD=", raw_text)
        self.assertIn("\nsuffix footer", raw_text)

        # No .orig backup written (no plaintext quarantine)
        backups = list(kb_compile.QUARANTINE_DIR.glob("*.orig")) if kb_compile.QUARANTINE_DIR.exists() else []
        self.assertEqual(len(backups), 0, "plaintext .orig backup must not be written")

        # Audit line in quarantine.log with metadata only
        self.assertTrue(kb_compile.QUARANTINE_LOG.exists(), "quarantine log not written")
        log_payload = kb_compile.QUARANTINE_LOG.read_text(encoding="utf-8")
        self.assertIn("id=1", log_payload)
        self.assertIn("source=compile", log_payload)
        self.assertIn("secret_assignment:redact", log_payload)
        self.assertNotIn("backup=", log_payload)
        self.assertNotIn("hunter2xyz", log_payload)
        self.assertNotIn(SECRET_TITLE, log_payload)

    def test_clean_entry_writes_no_quarantine(self):
        with contextlib.redirect_stdout(io.StringIO()):
            kb_compile.sanitize_unembedded([(1, "note", "clean body", "clean title", "t", "", str(self.raw_path))])
        self.assertFalse(kb_compile.QUARANTINE_DIR.exists() and list(kb_compile.QUARANTINE_DIR.glob("*.orig")))
        self.assertFalse(kb_compile.QUARANTINE_LOG.exists() and kb_compile.QUARANTINE_LOG.stat().st_size > 0)

    def test_parity_rules_beszel_token(self):
        parity_file = pathlib.Path(__file__).parent / "secret_patterns_parity.json"
        self.assertTrue(parity_file.exists(), f"parity file {parity_file} missing")
        data = json.loads(parity_file.read_text(encoding="utf-8"))
        for tc in data["positives"]:
            want_pattern = tc.get("pattern", "beszel_token")
            with self.subTest(pos=tc["name"]):
                cleaned, hits = kb_compile.sanitize_secrets(tc["input"])
                self.assertIn(tc["want_marker"], cleaned, f"failed for {tc['name']}: {cleaned}")
                if "want_output" in tc:
                    self.assertEqual(cleaned, tc["want_output"], f"failed for {tc['name']}")
                if tc.get("rescan"):
                    rescanned, rescan_hits = kb_compile.sanitize_secrets(tc["want_output"])
                    self.assertEqual(rescanned, tc["want_output"], f"re-scan changed shared output for {tc['name']}")
                    self.assertEqual(rescan_hits, [], f"re-scan hit for {tc['name']}: {rescan_hits}")
                pattern_hits = [h for h in hits if h[0] == want_pattern]
                self.assertTrue(bool(pattern_hits), f"missing hit {want_pattern} in {hits}")
                if "captured" in tc:
                    self.assertEqual(pattern_hits[0][2], tc["captured"])
        for tc in data["negatives"]:
            want_pattern = tc.get("pattern", "beszel_token")
            with self.subTest(neg=tc["name"]):
                cleaned, hits = kb_compile.sanitize_secrets(tc["input"])
                self.assertEqual(cleaned, tc["input"], f"unexpected modification for {tc['name']}: {cleaned}")
                self.assertFalse(any(h[0] == want_pattern for h in hits), f"unexpected hit in {hits}")

    def test_allowed_exact_match_only(self):
        rules = kb_compile.load_secret_rules()
        keep = [
            "example",
            "changeme",
            "<REDACTED_SECRET>",
            "   example   ",
            "CHANGEME",
            "",
        ]
        for val in keep:
            with self.subTest(keep=val):
                self.assertTrue(kb_compile._allowed(rules, val), f"_allowed({val!r}) want True")

        redact = [
            "your_password_here",
            "<REDACTED_SECRET> leftover",
            "hunter2example",
            "sk-or-v1-abcexample",
            "notreal123",
            "correct horse example battery",
            "managed by example script",
        ]
        for val in redact:
            with self.subTest(redact=val):
                self.assertFalse(kb_compile._allowed(rules, val), f"_allowed({val!r}) want False")

    def test_rescan_redacted_output_unchanged_for_every_marker(self):
        markers = [
            "<REDACTED_SECRET>",
            "<REDACTED_API_KEY>",
            "<REDACTED_BESZEL_TOKEN>",
            "<REDACTED_URL_PASSWORD>",
            "<REDACTED_BASIC_AUTH>",
            "<REDACTED_TELEGRAM_TOKEN>",
        ]
        for marker in markers:
            with self.subTest(marker=marker):
                inp = f"password: {marker}"
                out, hits = kb_compile.sanitize_secrets(inp)
                self.assertEqual(out, inp, f"re-scanning {marker} mutated")
                self.assertEqual(len(hits), 0, f"re-scanning {marker} produced hits: {hits}")

    def test_tag_only_secret_sanitized_in_db_fts_embed_and_raw(self):
        """Secret present only in tags must be redacted in DB, FTS, embed args, and raw note."""
        db = sqlite3.connect(self.db_path)
        secret_tags = "homelab,POSTGRES_PASSWORD=hunter2xyz,ops"
        try:
            db.execute("DELETE FROM entries")
            db.execute(
                "INSERT INTO entries (id, type, content, title, tags, summary, raw_path)"
                " VALUES (2, 'note', 'clean body', 'clean title', ?, '', ?)",
                (secret_tags, str(self.raw_path)),
            )
            db.commit()
        finally:
            db.close()

        raw_fm = (
            "---\n"
            "type: note\n"
            f"title: {kb_compile.encode_frontmatter_value('clean title')}\n"
            f"tags: {kb_compile.encode_frontmatter_value(secret_tags)}\n"
            "saved: 2026-11-06T09:00:00\n"
            "---\n\n"
            "clean body"
        )
        self.raw_path.write_text(raw_fm, encoding="utf-8")

        rows = kb_compile.get_unembedded()
        self.assertEqual(len(rows), 1)
        with contextlib.redirect_stdout(io.StringIO()):
            cleaned_rows = kb_compile.sanitize_unembedded(rows)

        # 1. DB entries row
        db = sqlite3.connect(self.db_path)
        try:
            tags, title, content = db.execute("SELECT tags, title, content FROM entries WHERE id=2").fetchone()
            fts_count = db.execute("SELECT count(*) FROM entries_fts WHERE entries_fts MATCH 'hunter2xyz'").fetchone()[0]
        finally:
            db.close()
        self.assertNotIn("hunter2xyz", tags)
        self.assertIn("<REDACTED_SECRET>", tags)
        self.assertEqual(title, "clean title")
        self.assertEqual(content, "clean body")

        # 2. FTS after sanitize_unembedded
        self.assertEqual(fts_count, 0, "FTS still matched secret after tag redaction")

        # 3. Arguments passed to passage_embed and upsert
        mock_model = mock.Mock()
        mock_embed = mock.Mock()
        mock_embed.tolist.return_value = [0.1, 0.2]
        mock_model.passage_embed.return_value = [mock_embed]
        mock_coll = mock.Mock()

        with mock.patch.object(kb_compile, "get_embed_model", return_value=mock_model), \
             mock.patch.object(kb_compile, "get_chroma_collection", return_value=mock_coll), \
             contextlib.redirect_stdout(io.StringIO()):
            kb_compile.embed_entries(cleaned_rows)

        mock_model.passage_embed.assert_called_once()
        passed_texts = mock_model.passage_embed.call_args[0][0]
        self.assertEqual(len(passed_texts), 1)
        self.assertNotIn("hunter2xyz", passed_texts[0])
        self.assertIn("<REDACTED_SECRET>", passed_texts[0])

        mock_coll.upsert.assert_called_once()
        passed_metas = mock_coll.upsert.call_args[1]["metadatas"]
        self.assertEqual(len(passed_metas), 1)
        self.assertNotIn("hunter2xyz", passed_metas[0]["tags"])
        self.assertIn("<REDACTED_SECRET>", passed_metas[0]["tags"])

        # 4. Raw note after _record_quarantine
        raw_text = self.raw_path.read_text(encoding="utf-8")
        self.assertNotIn("hunter2xyz", raw_text)
        self.assertIn("<REDACTED_SECRET>", raw_text)

    def test_record_quarantine_frontmatter_multiword_secret(self):
        """Frontmatter title and tags with quoted multi-word secret must be sanitized in raw note."""
        multiword_rules = {
            "version": 1,
            "patterns": [
                {
                    "name": "secret_assignment",
                    "regex": r'(?i)(?:password|passwd|pwd|secret|client_secret|access[_-]?token|auth[_-]?token)["\']?[ \t]*[:=][ \t]*["\']?([^\s"\']+(?:[ \t]+[^\s"\']+)*)',
                    "placeholder": "<REDACTED_SECRET>",
                    "action": "redact",
                    "confidence": "low",
                    "capture_group": 1,
                    "min_len": 6,
                    "min_entropy": 0,
                }
            ],
            "allowlist": ["changeme", "example"],
        }
        rules_path = pathlib.Path(self.temp.name) / "multiword-rules.json"
        rules_path.write_text(json.dumps(multiword_rules), encoding="utf-8")

        raw_fm = (
            "---\n"
            "type: note\n"
            f"title: {kb_compile.encode_frontmatter_value('password: \"two words here\"')}\n"
            f"tags: {kb_compile.encode_frontmatter_value('password: \"two words here\"')}\n"
            "saved: 2026-11-06T09:00:00\n"
            "---\n\n"
            "body text"
        )
        self.raw_path.write_text(raw_fm, encoding="utf-8")

        orig_rules = kb_compile.SECRET_PATTERNS_FILE
        try:
            kb_compile.SECRET_PATTERNS_FILE = rules_path
            kb_compile._secret_rules = None
            kb_compile._record_quarantine(
                entry_id=3,
                raw_path=str(self.raw_path),
                hits=[("secret_assignment", "redact", "two words here")],
            )
        finally:
            kb_compile.SECRET_PATTERNS_FILE = orig_rules
            kb_compile._secret_rules = None

        raw_text = self.raw_path.read_text(encoding="utf-8")
        self.assertNotIn("two words here", raw_text)
        self.assertIn("<REDACTED_SECRET>", raw_text)
        # Verify it parses back with decode_frontmatter_value cleanly
        parts = raw_text.split("---", 2)
        self.assertGreaterEqual(len(parts), 3)
        fm = {}
        for line in parts[1].strip().split("\n"):
            if ":" in line:
                k, v = line.split(":", 1)
                fm[k.strip()] = kb_compile.decode_frontmatter_value(v)
        self.assertIn("<REDACTED_SECRET>", fm["title"])
        self.assertIn("<REDACTED_SECRET>", fm["tags"])

    def test_secret_assignment_multiword_cases(self):
        cases = [
            ("password: hunter2 extra words", "password: <REDACTED_SECRET>"),
            ("password: abc def", "password: <REDACTED_SECRET>"),
            ('password: "two words here"', 'password: "<REDACTED_SECRET>"'),
            ("password: rotated monthly", "password: <REDACTED_SECRET>"),
            ("PASSWORD=abc123 OTHER_SETTING=value", "PASSWORD=<REDACTED_SECRET> OTHER_SETTING=value"),
            ("FOO_PASSWORD=hunter2xyz", "FOO_PASSWORD=<REDACTED_SECRET>"),
            ("password:\nnext line", "password:\nnext line"),
            ("password:\r\nnext line", "password:\r\nnext line"),
            ("password: abc", "password: abc"),
        ]
        for inp, want in cases:
            with self.subTest(inp=inp):
                out, _ = kb_compile.sanitize_secrets(inp)
                self.assertEqual(out, want, f"sanitize_secrets({inp!r}) got {out!r}, want {want!r}")

    def test_secret_assignment_rescan_stability_in_raw_frontmatter(self):
        inputs = [
            "alpha bravo charlie delta echo foxtrot golf hotel india",
            " ".join(f"word{i:03d}" for i in range(1, 201)),
        ]
        for value in inputs:
            with self.subTest(sanitizer_words=len(value.split())):
                with contextlib.redirect_stderr(io.StringIO()) as err:
                    first, _ = kb_compile.sanitize_secrets("password: " + value)
                    second, second_hits = kb_compile.sanitize_secrets(first)
                    third, third_hits = kb_compile.sanitize_secrets(second)
                self.assertEqual(second, first)
                self.assertEqual(third, first)
                self.assertEqual(second_hits, [])
                self.assertEqual(third_hits, [])
                self.assertEqual(err.getvalue(), "")
        for ending in ("\n", "\r\n"):
            for value in inputs:
                raw = ending.join(("---", f"title: password: {value}", "type: note", "---", "body"))
                with self.subTest(ending=repr(ending), words=len(value.split())):
                    with contextlib.redirect_stderr(io.StringIO()) as err:
                        first = kb_compile._sanitize_raw_note(raw, entry_path="fixture.md")
                        second = kb_compile._sanitize_raw_note(first, entry_path="fixture.md")
                        third = kb_compile._sanitize_raw_note(second, entry_path="fixture.md")
                    self.assertEqual(second, first)
                    self.assertEqual(third, first)
                    self.assertEqual(err.getvalue(), "")

    def test_non_ascii_length_measure_parity(self):
        # 3 Cyrillic characters (6 bytes UTF-8) -> len() is 3 < min_len 6 -> must stay unredacted
        under_six = "password: пар"
        out, hits = kb_compile.sanitize_secrets(under_six)
        self.assertEqual(out, under_six)
        self.assertEqual(len(hits), 0)

        # 6 Cyrillic characters (12 bytes UTF-8) -> len() is 6 == min_len 6 -> must be redacted
        six_chars = "password: пароль"
        out, hits = kb_compile.sanitize_secrets(six_chars)
        self.assertEqual(out, "password: <REDACTED_SECRET>")
        self.assertGreater(len(hits), 0)

    def test_sanitize_raw_note_f1_cases(self):
        """F1: _sanitize_raw_note delimits on full lines only, handles escaped quotes and malformed values."""
        orig_rules = kb_compile.SECRET_PATTERNS_FILE
        shipped_rules = pathlib.Path(__file__).resolve().parent.parent / "config" / "secret_patterns.json"
        try:
            kb_compile.SECRET_PATTERNS_FILE = shipped_rules
            kb_compile._secret_rules = None

            # 1. '---' inside a title with a quoted multi-word value before and after it
            raw1 = (
                "---\n"
                'title: "password: \\"two words\\" --- secret: \\"other words\\""\n'
                'tags: "ops"\n'
                "---\n"
                "body text\n"
            )
            clean1 = kb_compile._sanitize_raw_note(raw1, entry_path="note1.md")
            self.assertNotIn("two words", clean1)
            self.assertNotIn("other words", clean1)
            self.assertIn("<REDACTED_SECRET>", clean1)
            # Assert title still decodes as valid JSON
            lines1 = clean1.splitlines()
            title_line1 = next(line for line in lines1 if line.startswith("title:"))
            title_val1 = title_line1.split(":", 1)[1].strip()
            decoded1 = json.loads(title_val1)
            self.assertEqual(
                decoded1,
                'password: "<REDACTED_SECRET>" --- secret: "<REDACTED_SECRET>"',
            )

            # 2. An escaped quote after the '---'
            raw2 = (
                "---\n"
                'title: "something --- with \\" escaped quote password: \\"hunter2 extra words\\""\n'
                "---\n"
                "body text\n"
            )
            clean2 = kb_compile._sanitize_raw_note(raw2, entry_path="note2.md")
            self.assertNotIn("hunter2 extra words", clean2)
            lines2 = clean2.splitlines()
            title_line2 = next(line for line in lines2 if line.startswith("title:"))
            title_val2 = title_line2.split(":", 1)[1].strip()
            decoded2 = json.loads(title_val2)
            self.assertEqual(
                decoded2,
                'something --- with " escaped quote password: "<REDACTED_SECRET>"',
            )

            # 3. A malformed quoted title containing a secret
            raw3 = (
                "---\n"
                'title: "malformed quote password: hunter2 extra words without closing quote\n'
                'tags: "ops"\n'
                "---\n"
                "body text\n"
            )
            with io.StringIO() as warn_buf, contextlib.redirect_stdout(warn_buf):
                clean3 = kb_compile._sanitize_raw_note(raw3, entry_path="malformed_note.md")
                warn_output = warn_buf.getvalue()
            self.assertNotIn("hunter2 extra words", clean3)
            self.assertIn("<REDACTED_SECRET>", clean3)
            self.assertIn("[WARN]", warn_output)
            self.assertIn("malformed_note.md", warn_output)
            self.assertNotIn("hunter2", warn_output)

            # 4. An unchanged note stays byte-identical (including CRLF)
            raw4 = "---\r\ntitle: \"Clean Note\"\r\ntags: \"clean, ops\"\r\n---\r\nBody line 1\r\nBody line 2\r\n"
            clean4 = kb_compile._sanitize_raw_note(raw4, entry_path="clean.md")
            self.assertEqual(clean4, raw4)
            self.assertEqual(clean4.encode("utf-8"), raw4.encode("utf-8"))
        finally:
            kb_compile.SECRET_PATTERNS_FILE = orig_rules
            kb_compile._secret_rules = None

    def test_allow_skip_logging_deduplicates_and_ignores_markers(self):
        """F4: Allow-skip logging suppresses redaction markers and logs at most once per pattern."""
        orig_rules = kb_compile.SECRET_PATTERNS_FILE
        shipped_rules = pathlib.Path(__file__).resolve().parent.parent / "config" / "secret_patterns.json"
        try:
            kb_compile.SECRET_PATTERNS_FILE = shipped_rules
            kb_compile._secret_rules = None

            # 1. Markers produce NO stderr logs
            with io.StringIO() as err_buf, contextlib.redirect_stderr(err_buf):
                kb_compile.sanitize_secrets("password: <REDACTED_SECRET> and token: <redacted_api_key>")
                err_output = err_buf.getvalue()
            self.assertEqual(err_output, "", f"Expected no stderr for markers, got: {err_output!r}")

            # 2. Multiple allowed values log at most once per pattern
            with io.StringIO() as err_buf, contextlib.redirect_stderr(err_buf):
                kb_compile.sanitize_secrets("password: example\npassword: example\n")
                err_output = err_buf.getvalue()
            count = err_output.count("allow-set skipped pattern secret_assignment")
            self.assertEqual(count, 1, f"Expected exactly 1 log line, got {count}: {err_output!r}")
            self.assertNotIn("example", err_output)
        finally:
            kb_compile.SECRET_PATTERNS_FILE = orig_rules
            kb_compile._secret_rules = None

    def test_sanitize_raw_note_unicode_line_separators(self):
        """U+2028 and U+2029 inside a JSON title must not split physical frontmatter lines."""
        orig_rules = kb_compile.SECRET_PATTERNS_FILE
        shipped_rules = pathlib.Path(__file__).resolve().parent.parent / "config" / "secret_patterns.json"
        try:
            kb_compile.SECRET_PATTERNS_FILE = shipped_rules
            kb_compile._secret_rules = None

            # 1. U+2028 and U+2029 inside a JSON title before a quoted multi-word secret value
            raw1 = (
                "---\n"
                'title: "title with \u2028 and \u2029 password: \\"secret value here\\""\n'
                'tags: "ops"\n'
                "---\n"
                "body text\n"
            )
            with io.StringIO() as warn_buf, contextlib.redirect_stdout(warn_buf):
                clean1 = kb_compile._sanitize_raw_note(raw1, entry_path="note1.md")
                warn_output1 = warn_buf.getvalue()
            self.assertEqual(warn_output1, "", f"Unexpected warning: {warn_output1}")
            self.assertNotIn("secret value here", clean1)
            self.assertIn("<REDACTED_SECRET>", clean1)
            lines1 = clean1.split("\n")
            title_line1 = next(line for line in lines1 if line.startswith("title:"))
            title_val1 = title_line1.split(":", 1)[1].strip()
            decoded1 = json.loads(title_val1)
            self.assertEqual(
                decoded1,
                'title with \u2028 and \u2029 password: "<REDACTED_SECRET>"',
            )

            # 2. Continuation after U+2028 that looks like `tags: ...`
            raw2 = (
                "---\n"
                'title: "title prefix \u2028tags: fake_tag password: \\"secret value here\\""\n'
                "---\n"
                "body text\n"
            )
            with io.StringIO() as warn_buf, contextlib.redirect_stdout(warn_buf):
                clean2 = kb_compile._sanitize_raw_note(raw2, entry_path="note2.md")
                warn_output2 = warn_buf.getvalue()
            self.assertEqual(warn_output2, "", f"Unexpected warning: {warn_output2}")
            self.assertNotIn("secret value here", clean2)
            self.assertIn("<REDACTED_SECRET>", clean2)
            lines2 = clean2.split("\n")
            title_line2 = next(line for line in lines2 if line.startswith("title:"))
            title_val2 = title_line2.split(":", 1)[1].strip()
            decoded2 = json.loads(title_val2)
            self.assertEqual(
                decoded2,
                'title prefix \u2028tags: fake_tag password: "<REDACTED_SECRET>"',
            )

            # 3. Clean note containing these characters stays byte-identical (including CRLF)
            raw3 = (
                "---\r\n"
                'title: "Clean \u2028 and \u2029 Note"\r\n'
                'tags: "clean, ops"\r\n'
                "---\r\n"
                "Body \u2028 line 1\r\n"
                "Body line 2\r\n"
            )
            clean3 = kb_compile._sanitize_raw_note(raw3, entry_path="clean.md")
            self.assertEqual(clean3, raw3)
            self.assertEqual(clean3.encode("utf-8"), raw3.encode("utf-8"))
        finally:
            kb_compile.SECRET_PATTERNS_FILE = orig_rules
            kb_compile._secret_rules = None


if __name__ == "__main__":
    unittest.main()
