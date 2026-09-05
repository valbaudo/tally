# Agent environment. Same base + same seed as the gate image, plus a baked
# Claude Code CLI so Harbor skips its own install step entirely: with no
# version pin, install() returns early when _INSTALL_CHECK_COMMAND
# (`command -v claude`) exits 0, so ensure_system_dependencies(apt-get) and
# the network bootstrap installer are never reached.
# A tag is not bytes; the pin is a claim about bytes, and it was undefined
# one level down.
FROM node:22-bookworm@sha256:8a34c4ab3ea2c5cd194f07e317b2a8f09461d3c8b05c4e34c8ccd56d56024c4d AS cli
RUN npm install -g @anthropic-ai/claude-code@2.1.259

# A tag is not bytes; the pin is a claim about bytes, and it was undefined
# one level down.
FROM python:3.12-bookworm@sha256:581429e3df12d76e6af4be5ab7d0e7fc2013eb57dc23d2de691411c8efdbb970

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
 # Wall clock in the commit hash.
 && GIT_AUTHOR_DATE=2000-01-01T00:00:00Z GIT_COMMITTER_DATE=2000-01-01T00:00:00Z \
    git commit -q -m "seed: average(), its tests, and ci.sh (CI is RED)" \
 # .git/index carries inode/ctime stat data, so the layer differs per build
 # even with an identical commit.
 && rm .git/index && git read-tree HEAD

RUN mkdir -p /app/outputs && chmod 0777 /app/outputs
