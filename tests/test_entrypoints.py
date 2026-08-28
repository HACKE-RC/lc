import os
import subprocess
import sys
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]


class EntrypointTests(unittest.TestCase):
    def test_checkout_launcher_reports_version(self):
        result = subprocess.run(
            [sys.executable, str(ROOT / "lc.py"), "--version"],
            check=True,
            capture_output=True,
            text=True,
        )
        self.assertEqual(result.stdout.strip(), "lc 0.1.0")

    def test_module_entrypoint_reports_version(self):
        result = subprocess.run(
            [sys.executable, "-m", "lc", "--version"],
            check=True,
            capture_output=True,
            text=True,
            env={**os.environ, "PYTHONPATH": str(ROOT / "src")},
        )
        self.assertEqual(result.stdout.strip(), "lc 0.1.0")
