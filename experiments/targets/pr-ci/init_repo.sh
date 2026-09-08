#!/bin/bash
# Regenerates ./repo -- a fresh, real git repository seeded with a failing
# test (RED state). This is what gets handed to an agent for the
# PR-that-makes-CI-pass protocol. Safe to re-run: wipes any previous ./repo.
#
# ./repo is gitignored on purpose: it's a real nested git repository
# (its own .git, its own commits), regenerated on demand rather than
# committed into tally's own history.
set -euo pipefail
cd "$(dirname "$0")"

rm -rf repo
mkdir repo
cp seed/calc.py seed/test_calc.py seed/ci.sh repo/
chmod +x repo/ci.sh

cd repo
git init -q
git config user.email "toy@tally.local"
git config user.name "tally toy"
git add calc.py
git commit -q -m "initial: add average()"
git add test_calc.py ci.sh
git commit -q -m "test: add regression tests for average() (currently failing)"

echo "repo initialized at $(pwd)"
git log --oneline
