#!/bin/bash
# Build-time soundness check: run inside the gate image while it is being
# built. If the oracle stops discriminating, the image fails to build.
set -uo pipefail
run() { ASAN_OPTIONS=detect_leaks=0 timeout 10 "$1" "$2" >/dev/null 2>&1; echo $?; }
bad=0
for spec in "povs/crash.pov:PASS" "povs/both.pov:FAIL" "povs/neither.pov:FAIL"; do
  p=/gate/${spec%%:*}; want=${spec##*:}
  v=$(run /gate/vuln_bin "$p"); f=$(run /gate/fixed_bin "$p")
  if [ "$v" -ne 0 ] && [ "$f" -eq 0 ]; then got=PASS; else got=FAIL; fi
  echo "selftest $p vuln_rc=$v fixed_rc=$f -> $got (want $want)"
  [ "$got" = "$want" ] || bad=1
done
[ "$bad" -eq 0 ] || { echo "SELFTEST FAILED"; exit 1; }
echo "SELFTEST OK (oracle discrimination)"

# Build-time contract check: prove /tests/test.sh reads its declared output at
# outputDir/<name> (harbor.go), not a stale hardcoded path.
want() { /tests/test.sh >/dev/null 2>&1; r=$(cat /logs/verifier/reward.json 2>/dev/null); rm -rf /logs
         [ "$r" = "{\"reward\": $1}" ] || { echo "SELFTEST FAILED: $2 -> ${r:-no reward.json}"; exit 1; }; }
# A sabotaged environment must produce NO reward.json: a 0 here would be a
# rejection of an agent nobody checked (harbor.go, outputDir).
abstain() { /tests/test.sh >/dev/null 2>&1; r=$(ls /logs/verifier/reward.json 2>/dev/null); rm -rf /logs
            [ -z "$r" ] || { echo "SELFTEST FAILED: $1 -> wrote reward.json, expected none"; exit 1; }; }
mkdir -p /app/outputs
want 0 "nothing at /app/outputs/pov.bin"
cp /gate/povs/crash.pov /app/outputs/pov.bin
want 1 "oracle at /app/outputs/pov.bin"
mv /gate/fixed_bin /gate/fixed_bin.off
abstain "oracle present but the gate's own fixed_bin is gone"
mv /gate/fixed_bin.off /gate/fixed_bin
rm -f /app/outputs/pov.bin
echo "SELFTEST OK (contract)"
