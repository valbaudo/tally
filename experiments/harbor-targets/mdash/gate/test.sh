#!/bin/bash
echo "=== mdash gate (baked into the digest-pinned verifier image) hostname=$(hostname) ==="
ls -l /app/outputs/exploit_result.json 2>&1 || echo "(no artifact at /app/outputs/exploit_result.json)"
python3 /opt/gate/gate.py
