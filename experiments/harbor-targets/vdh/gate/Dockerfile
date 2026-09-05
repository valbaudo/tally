FROM python:3.13-slim

# The gate re-derives everything it can: the repo under audit is baked in
# here, so the agent's findings.jsonl is treated purely as untrusted data.
# GROUND_TRUTH.md is deliberately NOT baked in -- this gate is a citation
# check, not an oracle.
COPY src /gate/repo/src
COPY test.sh /tests/test.sh
COPY check.py /gate/check.py
COPY oracle.jsonl /gate/oracle.jsonl
COPY selftest.sh /gate/selftest.sh
RUN mkdir -p /app/outputs
RUN chmod +x /tests/test.sh /gate/selftest.sh && /gate/selftest.sh
