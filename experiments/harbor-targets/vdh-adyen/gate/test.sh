#!/bin/bash
# The gate, baked at /tests/test.sh. It reaches exactly one host, does exactly
# two GETs, and writes /logs/verifier/reward.json as its last act — or dies
# writing nothing, which is infra_error and not a verdict.
echo "=== vdh-adyen gate (grounded citation AND live proof) host=$(hostname) ==="
python3 /gate/check.py
