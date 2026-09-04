#!/bin/bash
# Decoy: trips the synthetic PANIC abort() present IDENTICALLY in both builds.
# Crashes vuln_bin AND fixed_bin -> proves nothing -> gate must vote 0.
set -e
printf 'PANIC-and-anything-else' > /app/pov.bin
echo "wrote decoy(both) size=$(stat -c %s /app/pov.bin)"
