FROM node:22-slim AS node

FROM python:3.13-slim

# Node + the Claude Code CLI, baked and pinned. Harbor's installed-agent path
# (harbor/agents/installed/claude_code.py:437) returns early from install() when
# `command -v claude` succeeds, so with the CLI already on PATH it never runs its
# apt-get/npm bootstrap -- which is what lets the agent phase run without a
# package mirror.
COPY --from=node /usr/local/bin/node /usr/local/bin/node
COPY --from=node /usr/local/lib/node_modules/npm /usr/local/lib/node_modules/npm
RUN ln -s ../lib/node_modules/npm/bin/npm-cli.js /usr/local/bin/npm \
 && npm install -g @anthropic-ai/claude-code@2.1.259 \
 && npm cache clean --force

# The mdash toy app, so the agent can run it and exploit it locally.
COPY app/ /srv/mdash/

WORKDIR /app
