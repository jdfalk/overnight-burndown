#!/usr/bin/env python3
# file: scripts/validate-config.py
# version: 1.0.0
"""Validate a rendered burndown config.yaml and dump lines around the common
failure point (line 38) so CI failures are immediately diagnosable."""

from __future__ import annotations

import sys
import yaml


def main() -> int:
    if len(sys.argv) < 2:
        print("usage: validate-config.py <config.yaml>", file=sys.stderr)
        return 2

    path = sys.argv[1]
    try:
        with open(path) as f:
            raw = f.read()
    except OSError as e:
        print(f"validate-config: cannot read {path!r}: {e}", file=sys.stderr)
        return 1

    lines = raw.splitlines(keepends=True)
    print(f"Config has {len(lines)} lines total.")
    for i, line in enumerate(lines, 1):
        if 34 <= i <= 44:
            print(f"  line {i:3d}: {line!r}")

    try:
        data = yaml.safe_load(raw)
        keys = list(data.keys()) if isinstance(data, dict) else repr(data)
        print(f"Python yaml.safe_load OK — top-level keys: {keys}")
        return 0
    except yaml.YAMLError as e:
        print(f"Python yaml.safe_load FAILED: {e}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
