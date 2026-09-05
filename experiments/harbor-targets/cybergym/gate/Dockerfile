FROM ubuntu:24.04
RUN apt-get update \
 && apt-get install -y --no-install-recommends gcc libc6-dev \
 && rm -rf /var/lib/apt/lists/*
COPY vuln.c /gate/vuln.c
RUN gcc -fsanitize=address -g -O0 -o /gate/vuln_bin  /gate/vuln.c \
 && gcc -fsanitize=address -g -O0 -o /gate/fixed_bin /gate/vuln.c -DFIXED
COPY povs /gate/povs
COPY test.sh /tests/test.sh
COPY selftest.sh /gate/selftest.sh
RUN mkdir -p /app/outputs
RUN chmod +x /tests/test.sh /gate/selftest.sh && /gate/selftest.sh
