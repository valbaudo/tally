#!/bin/bash
# The CI check. One command, clear exit code: 0 = green, non-zero = red.
set -uo pipefail
cd "$(dirname "$0")"
python3 -m unittest -v test_calc.py
