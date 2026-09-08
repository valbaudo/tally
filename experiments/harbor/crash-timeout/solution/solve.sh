#!/bin/bash
echo "solve.sh: sleeping 60s but agent timeout_sec will be 10s" >&2
sleep 60
echo tally > /app/answer.txt
