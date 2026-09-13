import ast
import pathlib
import shutil
import subprocess
import tempfile
import unittest
from html.parser import HTMLParser


REPO = pathlib.Path(__file__).resolve().parent.parent
ATLAS = REPO / "runtime" / "kb_atlas.py"
TEMPLATE = REPO / "runtime" / "atlas_template.html"


class _ScriptCollector(HTMLParser):
    """Collect inline JS bodies; skips non-JS blocks like application/json."""

    def __init__(self):
        super().__init__(convert_charrefs=False)
        self.blocks = []
        self._current = None
        self._is_js = False

    def handle_starttag(self, tag, attrs):
        if tag != "script":
            return
        attrs = dict(attrs)
        script_type = (attrs.get("type") or "").strip().lower()
        self._is_js = script_type in ("", "text/javascript", "module")
        self._current = []

    def handle_endtag(self, tag):
        if tag == "script" and self._current is not None:
            if self._is_js:
                self.blocks.append("".join(self._current))
            self._current = None

    def handle_data(self, data):
        if self._current is not None:
            self._current.append(data)


class KbAtlasContractTests(unittest.TestCase):
    """Contract checks for the Atlas generator and template.

    kb_atlas.py imports numpy and chromadb, which live only in the venv-embed
    environment, so the module itself cannot be imported under the plain
    system interpreter that `make test` uses. These tests therefore verify
    source-level invariants instead of calling the module.
    """

    def test_template_has_single_placeholder(self):
        content = TEMPLATE.read_text(encoding="utf-8")
        self.assertEqual(
            content.count("__ATLAS_DATA__"), 1,
            "template must contain exactly one __ATLAS_DATA__ placeholder",
        )

    def test_template_data_wiring(self):
        content = TEMPLATE.read_text(encoding="utf-8")
        self.assertIn('id="atlas-data"', content, "missing atlas-data script block")
        self.assertIn(
            'getElementById("atlas-data")', content,
            "JS must read the atlas-data block",
        )

    def test_threshold_constants(self):
        tree = ast.parse(ATLAS.read_text(encoding="utf-8"))
        constants = {}
        for node in ast.walk(tree):
            if isinstance(node, ast.Assign):
                for target in node.targets:
                    if isinstance(target, ast.Name) and target.id.isupper():
                        try:
                            constants[target.id] = ast.literal_eval(node.value)
                        except ValueError:
                            pass
        self.assertIn("REDUNDANCY_MAX", constants)
        self.assertIn("ISOLATED_MIN", constants)
        self.assertGreater(constants["REDUNDANCY_MAX"], 0.0)
        self.assertLess(
            constants["REDUNDANCY_MAX"], constants["ISOLATED_MIN"],
            "redundancy threshold must sit below the isolation threshold",
        )
        self.assertLess(constants["ISOLATED_MIN"], 1.0)
        self.assertGreaterEqual(constants.get("PREVIEW_CHARS", 0), 1)
        self.assertGreaterEqual(constants.get("NEIGHBORS", 0), 1)
        self.assertIsInstance(constants.get("SEED"), int)

    def test_shebang_is_venv_embed_python(self):
        """The daily rebuild unit runs this file directly via its shebang."""
        first_line = ATLAS.read_text(encoding="utf-8").splitlines()[0]
        self.assertEqual(first_line, "#!/opt/kb/venv-embed/bin/python")

    def test_inline_js_is_syntactically_valid(self):
        """node --check over each inline JS block (skip when node is absent).

        Guards against the class of breakage where an edit leaves the script
        block unparseable — e.g. a `const` used before its definition (temporal
        dead zone), which kills the whole page silently.
        """
        node = shutil.which("node")
        if node is None:
            self.skipTest("node not available")
        collector = _ScriptCollector()
        collector.feed(TEMPLATE.read_text(encoding="utf-8"))
        self.assertGreaterEqual(
            len(collector.blocks), 1, "template must contain at least one JS block"
        )
        with tempfile.TemporaryDirectory() as tmp:
            for i, block in enumerate(collector.blocks):
                path = pathlib.Path(tmp) / f"block{i}.js"
                path.write_text(block, encoding="utf-8")
                result = subprocess.run(
                    [node, "--check", str(path)],
                    capture_output=True, text=True,
                )
                self.assertEqual(
                    result.returncode, 0,
                    f"JS block {i} fails node --check:\n{result.stderr}",
                )


if __name__ == "__main__":
    unittest.main()
