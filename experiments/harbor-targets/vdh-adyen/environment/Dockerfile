# Agent environment for the Adyen VDH target. Same shape as the other targets:
# the task's input tree is baked in, so naming this image is naming the task.
#
# The agent gets the plugin source and NOTHING ELSE — no network beyond the
# CLI's own two hosts, and no reach to the live deployment. It hunts over
# bytes; only the gate touches the running system. That is the trust position
# the whole target rests on: the untrusted party is never on the wire.
FROM node:22-bookworm@sha256:8a34c4ab3ea2c5cd194f07e317b2a8f09461d3c8b05c4e34c8ccd56d56024c4d AS cli
RUN npm install -g @anthropic-ai/claude-code@2.1.259

FROM python:3.12-bookworm@sha256:581429e3df12d76e6af4be5ab7d0e7fc2013eb57dc23d2de691411c8efdbb970

COPY --from=cli /usr/local/bin/node /usr/local/bin/node
COPY --from=cli /usr/local/lib/node_modules/@anthropic-ai/claude-code/bin/claude.exe /usr/local/bin/claude

# adyen-shopware6 at e3b9bdfa1fb31d704619346a3d38598ade29d92c (2026-08-19).
# Pinned by commit in the README; the image digest is what dawn pins.
COPY repo /app/repo
RUN mkdir -p /app/outputs && chmod 0777 /app/outputs
