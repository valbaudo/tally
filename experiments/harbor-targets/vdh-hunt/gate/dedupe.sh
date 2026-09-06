#!/bin/bash
echo "=== vdh dedupe gate host=$(hostname) ==="
python3 /gate/dedupe_check.py
