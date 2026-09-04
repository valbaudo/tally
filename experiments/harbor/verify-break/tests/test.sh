#!/bin/bash
mkdir -p /logs/verifier
echo "=== verifier running. hostname=$(hostname) ==="
echo "reward.json present at verify time? $([ -f /logs/verifier/reward.json ] && echo YES || echo NO)"
if [ "$(cat /app/answer.txt 2>/dev/null)" = "dawn" ]; then
  echo 1 > /logs/verifier/reward.txt
else
  echo "VERDICT: task NOT solved -> 0"
  echo 0 > /logs/verifier/reward.txt
fi
