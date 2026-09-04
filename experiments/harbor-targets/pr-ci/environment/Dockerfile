# Agent environment. Same base + same seed as the gate image.
FROM python:3.12-bookworm

WORKDIR /app
COPY calc.py test_calc.py ci.sh /app/repo/
RUN chmod +x /app/repo/ci.sh \
 && cd /app/repo \
 && git init -q -b main \
 && git config user.email toy@dawn.local \
 && git config user.name  "dawn toy" \
 && git add -A \
 && git commit -q -m "seed: average(), its tests, and ci.sh (CI is RED)"
