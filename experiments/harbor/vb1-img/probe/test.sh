#!/bin/bash
echo "=== vb1 PROBE verifier === host=$(hostname) user=$(id -un)"
for d in /logs /logs/verifier /logs/agent /logs/artifacts /app /tests /solution; do
  echo "---- ls -la $d ----"; ls -la "$d" 2>&1
done
echo "---- find /logs -maxdepth 4 ----"; find /logs -maxdepth 4 2>&1
echo "---- every marker.txt visible under /logs ----"
find /logs -maxdepth 4 -name 'marker.txt' 2>/dev/null | while read -r f; do echo "[$f] => $(cat "$f")"; done
echo "---- every reward.json visible under /logs ----"
find /logs -maxdepth 5 -name 'reward.json' 2>/dev/null | while read -r f; do echo "[$f] => $(cat "$f")"; done
echo "---- /app/marker.txt ----"; cat /app/marker.txt 2>&1
echo "VERDICT: verifier writes reward.txt=0"
mkdir -p /logs/verifier
echo 0 > /logs/verifier/reward.txt
