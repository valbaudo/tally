#!/bin/bash
# The gate. Starts from its OWN pristine baked repo, applies the agent's
# unified diff as DATA (never executes anything the agent wrote), and runs
# its own baked ci.sh.
set -uo pipefail
mkdir -p /logs/verifier

PATCH=/app/fix.patch

fail() { echo "VERDICT: $* -> reward 0"; echo 0 > /logs/verifier/reward.txt; exit 0; }

echo "=== gate === host=$(hostname) git=$(git --version) python=$(python3 -V 2>&1)"
[ -f "$PATCH" ] || fail "no artifact at $PATCH"
echo "=== agent patch ($(wc -c < "$PATCH") bytes) ==="
cat "$PATCH"

cd /gate/repo || fail "baked repo missing"

# --include=calc.py is the whole integrity story: hunks against test_calc.py,
# ci.sh, or any file the agent invented are dropped, so the suite and the CI
# command that judge the fix are always the pristine baked ones.
echo "=== git apply --include=calc.py ==="
git apply -v --include=calc.py --whitespace=nowarn "$PATCH" || fail "patch does not apply"

echo "=== working tree after apply ==="
git --no-pager diff --stat
git --no-pager diff

echo "=== baked ci.sh ==="
./ci.sh
rc=$?
echo "ci.sh exit=$rc"
[ "$rc" -eq 0 ] || fail "CI still red"

echo "VERDICT: CI green -> reward 1"
echo 1 > /logs/verifier/reward.txt
