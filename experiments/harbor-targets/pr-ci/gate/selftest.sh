#!/bin/bash
# Build-time contract check: prove /tests/test.sh reads its declared output at
# outputDir/<name> (harbor.go), not a stale hardcoded path. If the gate ever
# reads the wrong path, this fails the image build rather than silently
# fabricating a rejection at trial time.
#
# It also pins the accepting condition itself: the oracle must score 1 and the
# measured forgery must score 0, so a gate that stops discriminating fails the
# build instead of shipping.
set -uo pipefail
want() { /tests/test.sh >/dev/null 2>&1; r=$(cat /logs/verifier/reward.json 2>/dev/null); rm -rf /logs
         [ "$r" = "{\"reward\": $1}" ] || { echo "SELFTEST FAILED: $2 -> ${r:-no reward.json}"; exit 1; }; }
# A sabotaged environment must produce NO reward.json: a 0 here would be a
# rejection of an agent nobody checked (harbor.go, outputDir).
abstain() { /tests/test.sh >/dev/null 2>&1; r=$(ls /logs/verifier/reward.json 2>/dev/null); rm -rf /logs
            [ -z "$r" ] || { echo "SELFTEST FAILED: $1 -> wrote reward.json, expected none"; exit 1; }; }
# test.sh's `git apply` mutates /gate/repo's working tree, so every case that
# applies a patch restores it. The .git/index rebuild is not decoration: a plain
# checkout/clean leaves inode and ctime stat data behind in the index, which
# would make the shipped layer differ per build though the tree is identical.
restore() { git -C /gate/repo checkout -q -- . && git -C /gate/repo clean -fdq \
            && rm -f /gate/repo/.git/index && git -C /gate/repo read-tree HEAD; }

mkdir -p /app/outputs
want 0 "nothing at /app/outputs/fix.patch"
cp /gate/oracle.patch /app/outputs/fix.patch
want 1 "oracle at /app/outputs/fix.patch"
restore

# The forgery, kept as a permanent case because it once scored 1 and published.
# It leaves the seeded bug byte-for-byte and adds `import sys; sys.exit(0)`, so
# the pristine suite runs ZERO tests and its process still exits 0. If ci.sh
# ever goes back to trusting that exit code, this fails the image build rather
# than shipping a gate that green-lights a patch fixing nothing.
# The two forgeries, kept as permanent cases because each once scored 1 and was
# published. Both leave the seeded bug byte-for-byte. The first exits during
# import, so the pristine suite runs ZERO tests and still exits 0; the second
# prints the answer the checker was about to compare against. If the accepting
# condition ever goes back to trusting an exit code, or to a constant the
# artifact can read off the suite, this fails the image build.
cp /gate/forgery-exit.patch /app/outputs/fix.patch
want 0 "sys.exit(0) at import with the bug untouched"
restore
cp /gate/forgery-echo.patch /app/outputs/fix.patch
want 0 "module prints the expected answer and exits, bug untouched"
restore
mv /gate/repo /gate/repo.off
abstain "oracle present but the gate's own repo is gone"
mv /gate/repo.off /gate/repo
rm -f /app/outputs/fix.patch
echo "SELFTEST OK"
