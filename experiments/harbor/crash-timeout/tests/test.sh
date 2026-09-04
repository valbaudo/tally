#!/bin/bash
# Self-contained: no network, no package installs.
mkdir -p /logs/verifier
if [ "$(cat /app/answer.txt 2>/dev/null)" = "dawn" ]; then
  echo "PASS: /app/answer.txt == dawn"
  echo 1 > /logs/verifier/reward.txt
else
  echo "FAIL: got '$(cat /app/answer.txt 2>/dev/null)'"
  echo 0 > /logs/verifier/reward.txt
fi
