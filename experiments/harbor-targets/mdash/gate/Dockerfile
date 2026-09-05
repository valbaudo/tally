FROM python:3.13-slim

# The app itself, baked in: the gate re-derives ground truth from this copy,
# never from anything the agent sends.
COPY app/ /opt/mdash/
COPY gate.py /opt/gate/gate.py
COPY test.sh /tests/test.sh
COPY oracle.json /gate/oracle.json
COPY selftest.sh /gate/selftest.sh
RUN mkdir -p /app/outputs
RUN chmod +x /tests/test.sh /gate/selftest.sh && /gate/selftest.sh
