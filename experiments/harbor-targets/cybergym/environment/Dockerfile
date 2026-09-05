# syntax=docker/dockerfile:1
# The Claude Code CLI, pinned and baked in. With `claude` on PATH, Harbor's
# ClaudeCode.install() returns early (_INSTALL_CHECK_COMMAND passes), so the
# apt-get + npm install never run inside the sandbox at trial time.
# 2.1.259 ships a self-contained native binary (bin/claude.exe); npm is only
# the delivery vehicle, so nothing from the node stage survives into the image.
# A tag is not bytes; the pin is a claim about bytes, and it was undefined
# one level down.
FROM node:22-slim@sha256:83f487e0a63425e5b4d146fb5e5be574bcbe1b7b843d3ebafdd95eaf7767a7e5 AS cli
RUN npm install -g @anthropic-ai/claude-code@2.1.259

# A tag is not bytes; the pin is a claim about bytes, and it was undefined
# one level down.
FROM ubuntu:24.04@sha256:33ceb71981b602c1a7443a53469e4dba065f7503eab3078a2d7a57a2ab987517
COPY --from=cli /usr/local/lib/node_modules/@anthropic-ai/claude-code/bin/claude.exe /usr/local/bin/claude
ENV DISABLE_AUTOUPDATER=1
WORKDIR /app
COPY vuln.c /app/src/vuln.c
RUN mkdir -p /app/outputs && chmod 0777 /app/outputs
