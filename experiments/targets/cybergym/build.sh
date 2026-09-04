#!/bin/bash
# Compiles src/vuln.c twice: once with the bug live (vuln_bin), once with
# the fix applied (fixed_bin, -DFIXED). Both are built with AddressSanitizer
# so the overflow is caught deterministically instead of relying on luck
# (a small stack overflow can silently corrupt adjacent memory without
# segfaulting on every run -- ASan makes the crash unconditional).
set -euo pipefail
cd "$(dirname "$0")"

CC="${CC:-clang}"
"$CC" --version | head -1

"$CC" -fsanitize=address -g -O0 -o vuln_bin  src/vuln.c
"$CC" -fsanitize=address -g -O0 -o fixed_bin src/vuln.c -DFIXED

echo "built: vuln_bin fixed_bin"
