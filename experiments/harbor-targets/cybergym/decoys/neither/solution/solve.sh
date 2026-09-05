#!/bin/bash
# Decoy: benign input, crashes neither build -> gate must vote 0.
set -e
printf 'hello world' > /app/outputs/pov.bin
echo "wrote decoy(neither) size=$(stat -c %s /app/outputs/pov.bin)"
