# Verifier image. Carries its OWN copy of the plugin source, so the agent's
# citation is checked against bytes the agent never touched, and dawn's verdict
# script baked at /tests/test.sh — a custom verifier image never receives
# Harbor's injected /tests.
#
# This is the first gate in the tree that reaches a network at all. dawn writes
# the host into the task's allowed_hosts, so the container's egress policy is
# what bounds it; TARGET in check.py is what bounds where it tries.
FROM python:3.12-bookworm@sha256:581429e3df12d76e6af4be5ab7d0e7fc2013eb57dc23d2de691411c8efdbb970

COPY repo /gate/repo
COPY gate/check.py /gate/check.py
COPY gate/test.sh /tests/test.sh
COPY gate/selftest.sh /gate/selftest.sh
RUN mkdir -p /app/outputs
RUN chmod +x /tests/test.sh /gate/selftest.sh && /gate/selftest.sh
