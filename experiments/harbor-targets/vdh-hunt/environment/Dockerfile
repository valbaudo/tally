# Agent environment. Carries the target source and a baked Claude Code CLI so
# Harbor skips its own install step.
#
# It does NOT carry the ground truth, and that absence is the point: the report
# stage is scored on recall and precision against a key that exists only in the
# gate image. A tag is not bytes; the pin is a claim about bytes.
FROM node:22-bookworm@sha256:8a34c4ab3ea2c5cd194f07e317b2a8f09461d3c8b05c4e34c8ccd56d56024c4d AS cli
RUN npm install -g @anthropic-ai/claude-code@2.1.259

FROM python:3.12-bookworm@sha256:581429e3df12d76e6af4be5ab7d0e7fc2013eb57dc23d2de691411c8efdbb970
COPY --from=cli /usr/local/bin/node /usr/local/bin/node
COPY --from=cli /usr/local/lib/node_modules/@anthropic-ai/claude-code/bin/claude.exe /usr/local/bin/claude

COPY src/ /app/src/
WORKDIR /app
RUN mkdir -p /app/outputs && chmod 0777 /app/outputs
