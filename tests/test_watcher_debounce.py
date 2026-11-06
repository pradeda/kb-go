import os
from pathlib import Path
import subprocess
import tempfile
import unittest


class WatcherDebounceTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.script_path = Path(__file__).resolve().parent.parent / "runtime" / "watcher.sh"
        if not cls.script_path.is_file():
            raise FileNotFoundError(f"Missing watcher.sh at {cls.script_path}")

        # Extract the real debounce block directly from runtime/watcher.sh
        content = cls.script_path.read_text(encoding="utf-8")
        block_lines = []
        recording = False
        saw_elapsed = False
        for line in content.splitlines():
            if "now=$(date +%s)" in line:
                recording = True
            if recording:
                block_lines.append(line)
                if "elapsed=" in line:
                    saw_elapsed = True
                if saw_elapsed and line.strip() == "fi":
                    break
        cls.debounce_code = "\n".join(block_lines)
        if "now=$(date +%s)" not in cls.debounce_code or "elapsed=" not in cls.debounce_code:
            raise ValueError(f"Failed to extract debounce block from {cls.script_path}")

    def run_debounce(self, state_content: str | None):
        with tempfile.TemporaryDirectory() as tmp:
            state_file = Path(tmp) / "state"
            if state_content is not None:
                state_file.write_text(state_content, encoding="utf-8")

            marker = Path(tmp) / "MARKER"
            # Format marker into payload if placeholder present
            if state_content is not None and "{MARKER}" in state_content:
                state_file.write_text(state_content.replace("{MARKER}", str(marker)), encoding="utf-8")

            test_script = f"""
set -u
min_interval=5
state="{state_file}"
slept=""
sleep() {{ slept="$1"; }}
{self.debounce_code}
printf "elapsed=%s slept=%s\\n" "$elapsed" "$slept"
"""
            res = subprocess.run(
                ["bash", "-c", test_script],
                capture_output=True,
                text=True,
            )
            marker_created = marker.exists()
            return res, marker_created

    def test_arithmetic_injection_does_not_execute_commands(self):
        """now[$(touch MARKER)0] must be sanitized to last=0 and never execute."""
        res, marker_created = self.run_debounce("now[$(touch {MARKER})0]")
        self.assertEqual(res.returncode, 0, f"Debounce failed: {res.stderr}")
        self.assertFalse(marker_created, "Command injection succeeded: MARKER was created!")
        self.assertIn("slept=", res.stdout)

    def test_octal_prevention_on_leading_zero(self):
        """09 must not cause bash octal base error, must be sanitized to last=0."""
        res, _ = self.run_debounce("09")
        self.assertEqual(res.returncode, 0, f"09 caused bash error: {res.stderr}")
        self.assertNotIn("value too great for base", res.stderr)
        self.assertIn("slept=\n", res.stdout)

    def test_overflow_prevention_on_20_digit_timestamp(self):
        """20-digit number must be rejected (max 19 digits), preventing 64-bit int overflow."""
        res, _ = self.run_debounce("99999999999999999999")
        self.assertEqual(res.returncode, 0, f"Overflow caused bash error: {res.stderr}")
        self.assertIn("slept=\n", res.stdout)

    def test_future_timestamp_treated_as_zero(self):
        """A future timestamp must be treated as 0 so it cannot suppress compiles indefinitely."""
        future_ts = 9999999999
        res, _ = self.run_debounce(str(future_ts))
        self.assertEqual(res.returncode, 0, f"Future timestamp failed: {res.stderr}")
        # When last=0, elapsed = now (~1.7e9) > min_interval (5), so sleep is not called
        self.assertIn("slept=\n", res.stdout, "Future timestamp triggered sleep instead of being reset to 0")

    def test_valid_recent_timestamp_debounces(self):
        """A valid recent timestamp (2 seconds ago) must sleep for remaining interval (3s)."""
        now = int(subprocess.check_output(["date", "+%s"]).decode().strip())
        res, _ = self.run_debounce(str(now - 2))
        self.assertEqual(res.returncode, 0, f"Recent timestamp failed: {res.stderr}")
        self.assertIn("elapsed=2", res.stdout)
        self.assertIn("slept=3", res.stdout)

    def test_both_corpus_profiles_share_code(self):
        """Verify profile contract for both homelab and ai corpora."""
        for corpus in ("homelab", "ai"):
            result = subprocess.run(
                ["bash", str(self.script_path), "--corpus", corpus, "--show-profile"],
                check=True,
                text=True,
                capture_output=True,
            )
            profile = dict(line.split("=", 1) for line in result.stdout.strip().splitlines())
            self.assertEqual(profile["corpus"], corpus)
            self.assertTrue(profile["state"].startswith("/tmp/"))


if __name__ == "__main__":
    unittest.main()
