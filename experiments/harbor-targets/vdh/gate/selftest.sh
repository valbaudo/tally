#!/bin/bash
# Build-time contract check: prove /tests/test.sh reads its declared output at
# outputDir/<name> (harbor.go), not a stale hardcoded path. If the gate ever
# reads the wrong path, this fails the image build rather than silently
# fabricating a rejection at trial time.
set -uo pipefail
want() { /tests/test.sh >/dev/null 2>&1; r=$(cat /logs/verifier/reward.json 2>/dev/null); rm -rf /logs
         [ "$r" = "{\"reward\": $1}" ] || { echo "SELFTEST FAILED: $2 -> ${r:-no reward.json}"; exit 1; }; }
mkdir -p /app/outputs
want 0 "nothing at /app/outputs/findings.jsonl"
cp /gate/oracle.jsonl /app/outputs/findings.jsonl
want 1 "oracle at /app/outputs/findings.jsonl"
rm -f /app/outputs/findings.jsonl
echo "SELFTEST OK"
