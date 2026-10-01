"""The isolation contract must not drift between the Go binary and compile.py.

corpus.go and compile.py each carry the list of environment variables that redirect
every writable path away from production, and the comment on both sides claims they
are mirrors. Nothing enforced that before: adding a variable on one side silently kept
the other on a production path, which is exactly the failure isolation exists to
prevent. This test reads both sources and binds them.
"""
import importlib.util
import os
import pathlib
import re
import shutil
import sys
import tempfile
import unittest
from unittest import mock

HERE = pathlib.Path(__file__).resolve().parent
REPO = HERE.parent

CORPORA = ("HOMELAB", "AI")
REQUIRED_SUFFIXES = ("DB", "RAW", "QUARANTINE", "ENV")

# compile.py imports its sibling corpora/supersede_index by bare name, so the runtime
# directory has to be importable whether this file is run by `make test` (which sets
# PYTHONPATH) or directly.
if str(REPO / "runtime") not in sys.path:
    sys.path.insert(0, str(REPO / "runtime"))


def go_isolation_vars() -> list[str]:
    source = (REPO / "corpus.go").read_text(encoding="utf-8")
    block = re.search(r"var isolationVars = \[\]string\{(.*?)\n\}", source, re.S)
    if block is None:
        raise AssertionError("isolationVars not found in corpus.go")
    return sorted(re.findall(r'"([A-Z][A-Z0-9_]*)"', block.group(1)))


def python_isolation_vars() -> list[str]:
    source = (REPO / "runtime" / "compile.py").read_text(encoding="utf-8")
    block = re.search(r"^ISOLATION_VARS = \((.*?)^\)", source, re.S | re.M)
    if block is None:
        raise AssertionError("ISOLATION_VARS not found in runtime/compile.py")
    return sorted(re.findall(r'"([A-Z][A-Z0-9_]*)"', block.group(1)))


class IsolationContractTests(unittest.TestCase):
    def test_the_two_lists_are_identical(self) -> None:
        self.assertEqual(go_isolation_vars(), python_isolation_vars())

    def test_every_writable_path_is_covered_for_both_corpora(self) -> None:
        """Database, raw tree, quarantine and env file, for each corpus."""
        expected = [f"KB_{corpus}_{suffix}" for corpus in CORPORA for suffix in REQUIRED_SUFFIXES]
        self.assertEqual(go_isolation_vars(), sorted(expected))

    def test_every_declared_variable_is_actually_consumed(self) -> None:
        """A declared-but-unused variable would leave that path in production.

        compile.py applies the override from a (corpus, profile key, env var) table, so
        grepping for literal `present["KB_…"]` reads would no longer prove anything.
        Importing the module with the whole set pointed at unique scratch paths proves
        the stronger property directly: every declared variable is consumed and lands in
        the profile field it names — a table row bound to the wrong corpus or field, or
        a variable missing from it, leaves that field on its production path and fails
        here."""
        scratch = pathlib.Path(tempfile.mkdtemp(prefix="kb-isolation-contract-"))
        self.addCleanup(shutil.rmtree, scratch, ignore_errors=True)

        env, expected = {}, {}
        for corpus in CORPORA:
            for suffix, key in (("DB", "db"), ("RAW", "raw"), ("QUARANTINE", "quarantine_dir"), ("ENV", "env")):
                value = str(scratch / f"{corpus.lower()}-{key}")
                env[f"KB_{corpus}_{suffix}"] = value
                expected[(corpus.lower(), key)] = value

        spec = importlib.util.spec_from_file_location(
            "kb_compile_isolation_contract", REPO / "runtime" / "compile.py"
        )
        module = importlib.util.module_from_spec(spec)
        with mock.patch.dict(os.environ, env):
            spec.loader.exec_module(module)

        self.assertEqual(sorted(env), python_isolation_vars())
        for (corpus, key), value in expected.items():
            self.assertEqual(module.CORPUS_PROFILES[corpus][key], value, f"{corpus}/{key}")

        go_source = (REPO / "corpus.go").read_text(encoding="utf-8")
        go_used = set(re.findall(r'ov\["(KB_[A-Z0-9_]+)"\]', go_source))
        self.assertEqual(go_used, set(go_isolation_vars()))


if __name__ == "__main__":
    unittest.main()
