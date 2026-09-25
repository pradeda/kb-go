"""The isolation contract must not drift between the Go binary and compile.py.

corpus.go and compile.py each carry the list of environment variables that redirect
every writable path away from production, and the comment on both sides claims they
are mirrors. Nothing enforced that before: adding a variable on one side silently kept
the other on a production path, which is exactly the failure isolation exists to
prevent. This test reads both sources and binds them.
"""
import pathlib
import re
import unittest

HERE = pathlib.Path(__file__).resolve().parent
REPO = HERE.parent

CORPORA = ("HOMELAB", "AI")
REQUIRED_SUFFIXES = ("DB", "RAW", "QUARANTINE", "ENV")


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
        """A declared-but-unused variable would leave that path in production."""
        python_source = (REPO / "runtime" / "compile.py").read_text(encoding="utf-8")
        self.assertEqual(
            set(re.findall(r'present\["(KB_[A-Z0-9_]+)"\]', python_source)),
            set(python_isolation_vars()),
        )
        go_source = (REPO / "corpus.go").read_text(encoding="utf-8")
        go_used = set(re.findall(r'ov\["(KB_[A-Z0-9_]+)"\]', go_source))
        self.assertEqual(go_used, set(go_isolation_vars()))


if __name__ == "__main__":
    unittest.main()
