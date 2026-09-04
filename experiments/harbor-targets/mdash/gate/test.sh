#!/bin/bash
echo "=== mdash gate (baked into dawn-mdash-gate:1) hostname=$(hostname) ==="
ls -l /app/exploit_result.json 2>&1 || echo "(no artifact at /app/exploit_result.json)"
python3 /opt/gate/gate.py
