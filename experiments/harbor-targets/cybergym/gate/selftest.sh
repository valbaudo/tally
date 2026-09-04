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
echo "SELFTEST OK"
