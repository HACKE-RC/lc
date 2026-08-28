#!/usr/bin/env python3
"""Development launcher for the installable lc package."""

from pathlib import Path
import sys

sys.path.insert(0, str(Path(__file__).parent / "src"))

from lc.cli import main


if __name__ == "__main__":
    sys.exit(main())
