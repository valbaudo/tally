#!/bin/bash
echo "=== vb1 FALLBACK-ONLY verifier === host=$(hostname)"
echo "---- ls -la /logs/verifier ----"; ls -la /logs/verifier 2>&1
mkdir -p /logs/verifier
if [ -f /logs/verifier/reward.json ]; then
  echo "PRE-EXISTING reward.json FOUND -> trusting it: $(cat /logs/verifier/reward.json)"
else
  echo "no pre-existing reward.json -> writing reward.txt=0"
  echo 0 > /logs/verifier/reward.txt
fi
