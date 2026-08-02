import importlib.util
import pathlib
import sqlite3
import tempfile
import unittest


MODULE_PATH = pathlib.Path(__file__).with_name("provision_storage.py")
SPEC = importlib.util.spec_from_file_location("provision_storage", MODULE_PATH)
provision = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(provision)


class ProvisionStorageTests(unittest.TestCase):
    def test_metadata_merge_preserves_existing_values(self):
        existing = {"hnsw:space": "cosine", "operator_note": "keep-me"}
        merged = provision.merge_metadata(existing, "homelab", now="fixed")
        self.assertEqual(merged["hnsw:space"], "cosine")
        self.assertEqual(merged["operator_note"], "keep-me")
        self.assertEqual(merged["corpus"], "homelab")
        self.assertEqual(merged["created_at"], "fixed")

    def test_metadata_merge_rejects_conflicts(self):
        with self.assertRaises(RuntimeError):
            provision.merge_metadata({"hnsw:space": "l2"}, "homelab")
        with self.assertRaises(RuntimeError):
            provision.merge_metadata({"corpus": "homelab"}, "ai")

    def test_metadata_validation_requires_persisted_metric_key(self):
        metadata = provision.expected_metadata("homelab")
        metadata["created_at"] = "fixed"
        del metadata["hnsw:space"]
        with self.assertRaises(RuntimeError):
            provision.validate_metadata(metadata, "homelab")

    def test_configuration_validation_uses_actual_hnsw_space(self):
        provision.validate_configuration({"hnsw": {"space": "cosine"}}, "ai")
        with self.assertRaises(RuntimeError):
            provision.validate_configuration({"hnsw": {"space": "l2"}}, "ai")

    def test_schema_has_entries_fts_and_triggers(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = pathlib.Path(tmp) / "test.db"
            db = sqlite3.connect(path)
            try:
                db.executescript(provision.SCHEMA.read_text(encoding="utf-8"))
                db.execute(
                    "INSERT INTO entries(type, content, title, tags) VALUES ('note', 'marker body', 'marker', 'test')"
                )
                count = db.execute(
                    "SELECT COUNT(*) FROM entries_fts WHERE entries_fts MATCH 'marker'"
                ).fetchone()[0]
                self.assertEqual(count, 1)
                db.execute("UPDATE entries SET title='updated' WHERE id=1")
                count = db.execute(
                    "SELECT COUNT(*) FROM entries_fts WHERE entries_fts MATCH 'updated'"
                ).fetchone()[0]
                self.assertEqual(count, 1)
                db.execute("DELETE FROM entries WHERE id=1")
                self.assertEqual(db.execute("SELECT COUNT(*) FROM entries_fts").fetchone()[0], 0)
            finally:
                db.close()

    def test_prepare_rebuild_resets_missing_embedded_rows(self):
        class FakeCollection:
            name = "ai_kb_collection"
            id = "fake-id"
            metadata = {
                **provision.expected_metadata("ai"),
                "created_at": "fixed",
            }
            configuration_json = {"hnsw": {"space": "cosine"}}

            def get(self, include=None):
                return {"ids": []}

        class FakeClient:
            collection = FakeCollection()

            def list_collections(self):
                return [self.collection]

            def get_collection(self, name):
                self.assert_name(name)
                return self.collection

            @staticmethod
            def assert_name(name):
                if name != "ai_kb_collection":
                    raise AssertionError(name)

        with tempfile.TemporaryDirectory() as tmp:
            path = pathlib.Path(tmp) / "restore.db"
            db = sqlite3.connect(path)
            try:
                db.executescript(provision.SCHEMA.read_text(encoding="utf-8"))
                db.execute(
                    "INSERT INTO entries(type, content, embedded_at) VALUES ('note', 'marker', 'done')"
                )
                db.commit()
            finally:
                db.close()
            original = provision.PROFILES["ai"]["db"]
            provision.PROFILES["ai"]["db"] = path
            try:
                report = provision.prepare_ai_index_rebuild(FakeClient())
            finally:
                provision.PROFILES["ai"]["db"] = original
            self.assertEqual(report["reset_to_pending"], 1)
            db = sqlite3.connect(path)
            try:
                self.assertIsNone(db.execute("SELECT embedded_at FROM entries").fetchone()[0])
            finally:
                db.close()

    def test_prepare_rebuild_creates_missing_ai_collection(self):
        class FakeCollection:
            name = "ai_kb_collection"
            id = "fake-id"
            metadata = {
                **provision.expected_metadata("ai"),
                "created_at": "fixed",
            }
            configuration_json = {"hnsw": {"space": "cosine"}}

            def get(self, include=None):
                return {"ids": []}

        class FakeClient:
            def list_collections(self):
                return []

            def create_collection(self, name, metadata):
                self.created = (name, metadata)
                return FakeCollection()

        with tempfile.TemporaryDirectory() as tmp:
            path = pathlib.Path(tmp) / "restore.db"
            db = sqlite3.connect(path)
            try:
                db.executescript(provision.SCHEMA.read_text(encoding="utf-8"))
                db.execute(
                    "INSERT INTO entries(type, content, embedded_at) VALUES ('note', 'marker', 'done')"
                )
                db.commit()
            finally:
                db.close()
            original = provision.PROFILES["ai"]["db"]
            provision.PROFILES["ai"]["db"] = path
            try:
                report = provision.prepare_ai_index_rebuild(FakeClient())
            finally:
                provision.PROFILES["ai"]["db"] = original
            self.assertTrue(report["collection_created"])
            self.assertEqual(report["reset_to_pending"], 1)


if __name__ == "__main__":
    unittest.main()
