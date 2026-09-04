# syntax=docker/dockerfile:1

# Claude Code 2.1.259 ships as a self-contained native ELF, so node/npm are
# only needed to resolve the pinned version at build time -- not at runtime.
FROM node:22-slim AS claude-cli
RUN npm install -g @anthropic-ai/claude-code@2.1.259

FROM python:3.13-slim

# Harbor's ClaudeCode.install() returns early when `command -v claude`
# succeeds (_INSTALL_CHECK_COMMAND), so it never reaches
# ensure_system_dependencies(curl, bash, nodejs, npm, procps) -- no apt-get
# during agent setup, which is what lets the agent phase run on a narrow
# allowlist instead of needing a Debian mirror.
COPY --from=claude-cli \
  /usr/local/lib/node_modules/@anthropic-ai/claude-code/bin/claude.exe \
  /usr/local/bin/claude
RUN claude --version

WORKDIR /app
COPY src /app/repo/src
COPY demo_exploits.py /app/repo/demo_exploits.py
