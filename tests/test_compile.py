import importlib.util
import contextlib
import io
import pathlib
import sqlite3
import subprocess
import tempfile
import unittest
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


if __name__ == "__main__":
    unittest.main()
