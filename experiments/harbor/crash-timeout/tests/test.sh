#!/bin/bash
# Self-contained: no network, no package installs.
mkdir -p /logs/verifier
if [ "$(cat /app/answer.txt 2>/dev/null)" = "tally" ]; then
  echo "PASS: /app/answer.txt == tally"
  echo 1 > /logs/verifier/reward.txt
else
  echo "FAIL: got '$(cat /app/answer.txt 2>/dev/null)'"
  echo 0 > /logs/verifier/reward.txt
fi
