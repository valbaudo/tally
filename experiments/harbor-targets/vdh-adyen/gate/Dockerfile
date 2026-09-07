# Verifier image. Carries its OWN copy of the plugin source, so the agent's
# citation is checked against bytes the agent never touched, and dawn's verdict
# script baked at /tests/test.sh — a custom verifier image never receives
# Harbor's injected /tests.
#
# This is the first gate in the tree that reaches a network at all. dawn writes
# the host into the task's allowed_hosts, so the container's egress policy is
# what bounds it; TARGET in check.py is what bounds where it tries.
FROM python:3.12-bookworm@sha256:581429e3df12d76e6af4be5ab7d0e7fc2013eb57dc23d2de691411c8efdbb970
# This build RUNs the target's own code, and CPython writes __pycache__ beside
# every module it imports. Those .pyc files embed the source mtime, so the layer
# differed on every rebuild and the pinned digest was a fact about one build
# rather than about this tree — measured: five identical --no-cache builds of the
# vdh gates produced five different digests. This is the line that makes
# docker-bake.hcl's opening claim true for images that execute Python.
ENV PYTHONDONTWRITEBYTECODE=1

# The oracle, supplied at build time so the repo holds no secret. Default
# UNARMED: that image refuses to vote (see check.py), which is the honest state
# for a gate with no ground truth. Arming it changes these bytes, so an armed
# gate is a different digest -- which is correct, since it is a different
# oracle.
ARG CANARY=UNARMED

COPY repo /gate/repo
COPY gate/check.py /gate/check.py
RUN sed -i "s/^CANARY = .*/CANARY = \"$CANARY\"/" /gate/check.py
COPY gate/test.sh /tests/test.sh
COPY gate/selftest.sh /gate/selftest.sh
RUN mkdir -p /app/outputs
RUN chmod +x /tests/test.sh /gate/selftest.sh && /gate/selftest.sh build
