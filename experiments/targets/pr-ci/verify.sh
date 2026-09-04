#!/bin/bash
# Demonstrates the pass condition end-to-end: a fresh repo starts RED, a
# real git commit (standing in for the agent's PR) applies fix.patch, the
# repo ends GREEN. Not itself the CI check -- that's repo/ci.sh, the single
# command an agent (or a harness) runs to ask "did it pass?". This script
# just proves the red -> green transition is real and that ci.sh
# discriminates between the two states.
set -uo pipefail
cd "$(dirname "$0")"

./init_repo.sh
echo
echo "=== CI at HEAD (expect RED / non-zero) ==="
(cd repo && ./ci.sh)
red_rc=$?
echo "ci.sh exit code: $red_rc"

echo
echo "=== applying fix.patch as a real commit (the mutation / PR) ==="
(cd repo && git apply ../fix.patch && git add calc.py && git commit -q -m "fix: remove stray -1 in average()")
(cd repo && git log --oneline)

echo
echo "=== CI at HEAD (expect GREEN / zero) ==="
(cd repo && ./ci.sh)
green_rc=$?
echo "ci.sh exit code: $green_rc"

echo
if [ "$red_rc" -ne 0 ] && [ "$green_rc" -eq 0 ]; then
    echo "PASS: went from red ($red_rc) to green ($green_rc) via a real commit"
    exit 0
else
    echo "FAIL: red_rc=$red_rc green_rc=$green_rc"
    exit 1
fi
