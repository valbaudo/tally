# Verifier image. Same base + same seed as the agent environment.
# The gate is BAKED IN at /tests/test.sh: a custom verifier image never
# receives Harbor's injected /tests.
FROM python:3.12-bookworm

COPY calc.py test_calc.py ci.sh /gate/repo/
RUN chmod +x /gate/repo/ci.sh \
 && cd /gate/repo \
 && git init -q -b main \
 && git config user.email toy@dawn.local \
 && git config user.name  "dawn toy" \
 && git add -A \
 && git commit -q -m "seed: average(), its tests, and ci.sh (CI is RED)" \
 && mkdir -p /app

COPY test.sh /tests/test.sh
RUN chmod +x /tests/test.sh
