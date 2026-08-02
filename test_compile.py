import importlib.util
import contextlib
import io
import pathlib
import subprocess
import tempfile
import unittest


MODULE_PATH = pathlib.Path(__file__).with_name("compile.py")
SPEC = importlib.util.spec_from_file_location("kb_compile", MODULE_PATH)
kb_compile = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(kb_compile)


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
        self.assertIn("--corpus ai", outputs["ai"]["compile"])

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
