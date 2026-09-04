#!/bin/bash
mkdir -p /logs/verifier
echo "=== vdh gate (citation/format check only) host=$(hostname) ==="
python3 /gate/check.py
