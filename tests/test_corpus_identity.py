"""Corpus identity must be one definition, and the two language sides must agree.

corpora.py is the source for every Python consumer (compile.py, kb_atlas.py,
provision_storage.py, kb_v2.py/kb_search_api.py, mcp_server.py, gate.py). The Go CLI
cannot import Python and keeps its own table in corpus.go, so this test binds the two:
the paths and collection names for both corpora have to match, and neither side may
carry a second copy of them.
"""
import importlib.util
import pathlib
import re
import unittest

HERE = pathlib.Path(__file__).resolve().parent
REPO = HERE.parent
RUNTIME = REPO / "runtime"

SPEC = importlib.util.spec_from_file_location("corpora_under_test", RUNTIME / "corpora.py")
corpora = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(corpora)

# The Go table is parsed from source: a Go test cannot import Python and a Python test
# cannot import Go, so the source text is the contract both sides are read from.
GO_FIELDS = {
    "DBPath": "db",
    "RawRoot": "raw",
    "EnvFile": "env",
    "ChromaCollection": "collection",
    "WikiIndexPath": "wiki_index",
    "SecretPatternsPath": "secret_patterns",
    "QuarantineDir": "quarantine_dir",
    "QuarantineLog": "quarantine_log",
    "WatcherLock": "watcher_lock",
    "WatcherState": "watcher_state",
}


def go_profiles() -> dict[str, dict[str, str]]:
    source = (REPO / "corpus.go").read_text(encoding="utf-8")
    blocks = re.findall(r"p = CorpusProfile\{(.*?)\n\t\t\}", source, re.S)
    if len(blocks) != 2:
        raise AssertionError(f"expected two CorpusProfile literals, found {len(blocks)}")
    profiles = {}
    for block in blocks:
        name_match = re.search(r'Name:\s+"([a-z]+)"', block)
        fields = dict(re.findall(r'(\w+):\s*"([^"]*)"', block))
        profiles[name_match.group(1)] = {key: fields[go] for go, key in GO_FIELDS.items()}
    return profiles


class CorpusIdentityTests(unittest.TestCase):
    def test_go_and_python_agree_on_every_path(self) -> None:
        go = go_profiles()
        python = corpora.CORPORA
        # "root" is a Python-side convenience key (the corpus root directory); the Go
        # profile carries no field for it, so only the shared keys are compared.
        shared = {name: {key: value for key, value in profile.items() if key != "root"}
                  for name, profile in python.items()}
        self.assertEqual(go, shared)

    def test_the_python_consumers_do_not_carry_their_own_copy(self) -> None:
        """Only corpora.py may name the paths; this catches a re-introduced literal."""
        offenders = []
        for path in sorted(RUNTIME.glob("*.py")):
            if path.name == "corpora.py":
                continue
            text = path.read_text(encoding="utf-8")
            if '"/opt/kb/kb.db"' in text or '"/opt/ai-kb/ai-kb.db"' in text:
                offenders.append(str(path.relative_to(REPO)))
        self.assertEqual(offenders, [])

    def test_helper_views_match_the_profiles(self) -> None:
        self.assertEqual(corpora.databases(), {
            name: profile["db"] for name, profile in corpora.CORPORA.items()
        })
        self.assertEqual(corpora.collections(), {
            name: profile["collection"] for name, profile in corpora.CORPORA.items()
        })

    def test_profiles_returns_a_copy(self) -> None:
        """compile.py's isolation override mutates its copy, not the definition."""
        copy_of = corpora.profiles()
        copy_of["homelab"]["db"] = "/tmp/other.db"
        self.assertEqual(corpora.CORPORA["homelab"]["db"], "/opt/kb/kb.db")


if __name__ == "__main__":
    unittest.main()
