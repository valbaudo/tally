#!/bin/bash
# Build-time contract check: prove /tests/test.sh reads its declared output at
# outputDir/<name> (harbor.go), not a stale hardcoded path. If the gate ever
# reads the wrong path, this fails the image build rather than silently
# fabricating a rejection at trial time.
#
# test.sh's `git apply` mutates /gate/repo's working tree, so the oracle run
# below leaves it fixed -- restored to the pristine seed commit before this
# script exits, so that mutation never survives into the shipped image layer.
set -uo pipefail
want() { /tests/test.sh >/dev/null 2>&1; r=$(cat /logs/verifier/reward.json 2>/dev/null); rm -rf /logs
         [ "$r" = "{\"reward\": $1}" ] || { echo "SELFTEST FAILED: $2 -> ${r:-no reward.json}"; exit 1; }; }
# A sabotaged environment must produce NO reward.json: a 0 here would be a
# rejection of an agent nobody checked (harbor.go, outputDir).
abstain() { /tests/test.sh >/dev/null 2>&1; r=$(ls /logs/verifier/reward.json 2>/dev/null); rm -rf /logs
            [ -z "$r" ] || { echo "SELFTEST FAILED: $1 -> wrote reward.json, expected none"; exit 1; }; }
mkdir -p /app/outputs
want 0 "nothing at /app/outputs/fix.patch"
cp /gate/oracle.patch /app/outputs/fix.patch
want 1 "oracle at /app/outputs/fix.patch"
git -C /gate/repo checkout -q -- . && git -C /gate/repo clean -fdq
mv /gate/repo /gate/repo.off
abstain "oracle present but the gate's own repo is gone"
mv /gate/repo.off /gate/repo
rm -f /app/outputs/fix.patch
echo "SELFTEST OK"
