# Verifier image. Same base + same seed as the agent environment.
# The gate is BAKED IN at /tests/test.sh: a custom verifier image never
# receives Harbor's injected /tests.
# A tag is not bytes; the pin is a claim about bytes, and it was undefined
# one level down.
FROM python:3.12-bookworm@sha256:581429e3df12d76e6af4be5ab7d0e7fc2013eb57dc23d2de691411c8efdbb970

COPY calc.py test_calc.py ci.sh /gate/repo/
RUN chmod +x /gate/repo/ci.sh \
 && cd /gate/repo \
 && git init -q -b main \
 && git config user.email toy@dawn.local \
 && git config user.name  "dawn toy" \
 && git add -A \
 # Wall clock in the commit hash.
 && GIT_AUTHOR_DATE=2000-01-01T00:00:00Z GIT_COMMITTER_DATE=2000-01-01T00:00:00Z \
    git commit -q -m "seed: average(), its tests, and ci.sh (CI is RED)" \
 # .git/index carries inode/ctime stat data, so the layer differs per build
 # even with an identical commit.
 && rm .git/index && git read-tree HEAD

COPY test.sh /tests/test.sh
COPY oracle.patch /gate/oracle.patch
COPY selftest.sh /gate/selftest.sh
RUN mkdir -p /app/outputs
RUN chmod +x /tests/test.sh /gate/selftest.sh && /gate/selftest.sh
