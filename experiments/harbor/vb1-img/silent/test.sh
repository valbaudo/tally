#!/bin/bash
echo "=== vb1 SILENT verifier === host=$(hostname)"
echo "---- ls -la /logs/verifier ----"; ls -la /logs/verifier 2>&1
echo "---- find /logs -maxdepth 4 ----"; find /logs -maxdepth 4 2>&1
echo "DELIBERATELY WRITING NO REWARD FILE"
