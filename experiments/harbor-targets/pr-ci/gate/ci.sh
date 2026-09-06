#!/bin/bash
# The CI check. One command, clear exit code -- but the exit code is THIS
# script's, never that of a process which imports the agent's calc.py.
set -uo pipefail
cd "$(dirname "$0")"

# Readable per-test detail. Diagnostics: its exit code decides nothing, because
# `import sys; sys.exit(0)` in calc.py makes it exit 0 having run no tests.
python3 -m unittest -v test_calc.py

# The verdict. See check.py for why it picks its inputs at run time.
python3 check.py
