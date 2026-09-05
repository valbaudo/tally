# A tag is not bytes; the pin is a claim about bytes, and it was undefined
# one level down.
FROM ubuntu:24.04@sha256:33ceb71981b602c1a7443a53469e4dba065f7503eab3078a2d7a57a2ab987517
RUN apt-get update \
 && apt-get install -y --no-install-recommends gcc libc6-dev \
 && rm -rf /var/lib/apt/lists/* \
 # apt/dpkg logs and the ldconfig cache carry this install's wall clock, not
 # the packages' bytes -- the layer differs build to build even though the
 # same package versions land every time.
 && rm -f /var/log/apt/history.log /var/log/apt/term.log /var/log/dpkg.log \
          /var/log/alternatives.log /var/cache/ldconfig/aux-cache
COPY vuln.c /gate/vuln.c
RUN gcc -fsanitize=address -g -O0 -o /gate/vuln_bin  /gate/vuln.c \
 && gcc -fsanitize=address -g -O0 -o /gate/fixed_bin /gate/vuln.c -DFIXED
COPY povs /gate/povs
COPY test.sh /tests/test.sh
COPY selftest.sh /gate/selftest.sh
RUN mkdir -p /app/outputs
# selftest.sh actually runs the gate here, and ASan's crash report embeds
# ASLR-randomized addresses -- test.sh's /tmp/*.out capture of that report
# would otherwise bake a different layer into the image on every build.
RUN chmod +x /tests/test.sh /gate/selftest.sh && /gate/selftest.sh \
 && rm -f /tmp/v.out /tmp/f.out
