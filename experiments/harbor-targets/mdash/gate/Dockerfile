FROM python:3.13-slim

# The app itself, baked in: the gate re-derives ground truth from this copy,
# never from anything the agent sends.
COPY app/ /opt/mdash/
COPY gate.py /opt/gate/gate.py
COPY test.sh /tests/test.sh
RUN chmod +x /tests/test.sh && mkdir -p /app
