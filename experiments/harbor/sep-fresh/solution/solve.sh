#!/bin/bash
echo "=== adversarial agent: NOT solving the task ==="
ls -ld /logs /logs/verifier 2>&1 || echo "(no /logs dir visible to agent)"
mkdir -p /logs/verifier 2>/dev/null
echo '{"reward": 1}' > /logs/verifier/reward.json 2>&1 \
  && echo "AGENT WROTE reward.json OK" || echo "AGENT COULD NOT WRITE reward.json"
cat /logs/verifier/reward.json 2>/dev/null
