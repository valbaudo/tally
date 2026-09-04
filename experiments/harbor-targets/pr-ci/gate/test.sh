#!/bin/bash
# The gate. Starts from its OWN pristine baked repo, applies the agent's
# unified diff as DATA (never executes anything the agent wrote), and runs
# its own baked ci.sh.
#
# Single exit path: /logs/verifier/reward.json is the LAST thing written, and
# nothing else writes a reward. Harbor restores declared artifacts into this
# container BEFORE the gate runs, so writing the highest-precedence reward file
# last is what makes the verdict unforgeable. If the gate dies before that
# line there is no reward.json at all, and the trial is an infra_error rather
# than a verdict.
set -uo pipefail
mkdir -p /logs/verifier/publish

PATCH=/app/fix.patch
PUBLISH=/logs/verifier/publish/fix.patch
reward=0

gate() {
  echo "=== gate === host=$(hostname) git=$(git --version) python=$(python3 -V 2>&1)"

  [ -f "$PATCH" ] || { echo "VERDICT: no artifact at $PATCH -> reward 0"; return; }
  echo "=== agent patch ($(wc -c < "$PATCH") bytes) ==="
  cat "$PATCH"

  cd /gate/repo || { echo "VERDICT: baked repo missing -> reward 0"; return; }

  # --include=calc.py is the whole integrity story: hunks against test_calc.py,
  # ci.sh, or any file the agent invented are dropped, so the suite and the CI
  # command that judge the fix are always the pristine baked ones.
  echo "=== git apply --include=calc.py ==="
  git apply -v --include=calc.py --whitespace=nowarn "$PATCH" \
    || { echo "VERDICT: patch does not apply -> reward 0"; return; }

  echo "=== working tree after apply ==="
  git --no-pager diff --stat
  git --no-pager diff

  echo "=== baked ci.sh ==="
  ./ci.sh
  rc=$?
  echo "ci.sh exit=$rc"
  [ "$rc" -eq 0 ] || { echo "VERDICT: CI still red -> reward 0"; return; }

  # The only bytes an actuator may push: the POST-FILTER diff, re-derived from
  # the gate's own pristine repo. NOT the agent's raw /app/fix.patch, which can
  # carry the hunks --include dropped (a neutered test_calc.py, an `exit 0`
  # appended to ci.sh). Publishing the raw patch would restore exactly the
  # attack the filter just defeated.
  git --no-pager diff > "$PUBLISH"
  echo "=== published $(wc -c < "$PUBLISH") bytes to $PUBLISH ==="

  echo "VERDICT: CI green -> reward 1"
  reward=1
}

gate
printf '{"reward": %d}\n' "$reward" > /logs/verifier/reward.json
cat /logs/verifier/reward.json
