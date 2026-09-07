# Verifier image. Carries its OWN copy of the app: the gate plants a fresh
# secret in a scratch copy of it at run time, so the note that proves the flaw
# is one no source the agent ever saw contains.
# A tag is not bytes; the pin is a claim about bytes, and it was undefined
# one level down.
FROM python:3.12-bookworm@sha256:581429e3df12d76e6af4be5ab7d0e7fc2013eb57dc23d2de691411c8efdbb970
# This build RUNs the target's own code, and CPython writes __pycache__ beside
# every module it imports. Those .pyc files embed the source mtime, so the layer
# differed on every rebuild and the pinned digest was a fact about one build
# rather than about this tree — measured: five identical --no-cache builds of the
# vdh gates produced five different digests. This is the line that makes
# docker-bake.hcl's opening claim true for images that execute Python.
ENV PYTHONDONTWRITEBYTECODE=1

COPY app/ /opt/mdash/
COPY gate.py /opt/gate/gate.py
COPY test.sh /tests/test.sh
COPY oracle.json /gate/oracle.json
COPY forgery.json /gate/forgery.json
COPY selftest.sh /gate/selftest.sh
RUN mkdir -p /app/outputs
RUN chmod +x /tests/test.sh /gate/selftest.sh && /gate/selftest.sh
