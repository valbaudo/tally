# Agent environment. Same base + same seed as the gate image, plus a baked
# Claude Code CLI so Harbor skips its own install step entirely: with no
# version pin, install() returns early when _INSTALL_CHECK_COMMAND
# (`command -v claude`) exits 0, so ensure_system_dependencies(apt-get) and
# the network bootstrap installer are never reached.
FROM node:22-bookworm AS cli
RUN npm install -g @anthropic-ai/claude-code@2.1.259

FROM python:3.12-bookworm

# claude.exe is a self-contained linux-arm64 native binary; node is kept only
# for anything the CLI shells out to (MCP stdio servers, npx bridges).
COPY --from=cli /usr/local/bin/node /usr/local/bin/node
COPY --from=cli /usr/local/lib/node_modules/@anthropic-ai/claude-code/bin/claude.exe /usr/local/bin/claude

WORKDIR /app
COPY calc.py test_calc.py ci.sh /app/repo/
RUN chmod +x /app/repo/ci.sh \
 && cd /app/repo \
 && git init -q -b main \
 && git config user.email toy@dawn.local \
 && git config user.name  "dawn toy" \
 && git add -A \
 && git commit -q -m "seed: average(), its tests, and ci.sh (CI is RED)"
