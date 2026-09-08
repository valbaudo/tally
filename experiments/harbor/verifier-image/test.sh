#!/bin/bash
mkdir -p /logs/verifier
echo "=== BAKED verifier ==="
echo "/app/answer.txt visible? $([ -f /app/answer.txt ] && cat /app/answer.txt || echo NO)"
echo "/app contents: $(ls /app 2>/dev/null | tr '\n' ' ' || echo '(no /app)')"
if [ "$(cat /app/answer.txt 2>/dev/null)" = "tally" ]; then echo 1 > /logs/verifier/reward.txt; else echo 0 > /logs/verifier/reward.txt; fi
