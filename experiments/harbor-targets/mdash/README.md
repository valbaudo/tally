# dawn/mdash -- a Harbor task around the mdash toy IDOR

> Pasted trial output in this file is literal, from the runs it names, so its
> paths predate the `/app/outputs` rename. It is evidence, not the contract —
> for that, see `outputDir` in `harbor.go`.


## What it demonstrates

That a **proving plugin can live entirely inside a pinned verifier image** and
still discriminate, under the two constraints the earlier experiments settled:
the verifier is `separate` (so it cannot see the agent's filesystem) and
`no-network`.

The agent's job is to *exercise* a broken-object-level-authorization flaw in
`/srv/mdash` (source: `experiments/targets/mdash/`): log in as `alice`, then
read **bob's** private note with alice's own token. It writes what it obtained
to `/app/outputs/exploit_result.json`.

The gate does **not** trust that file's claim of success. It bakes its own copy
of the service at `/opt/mdash`, starts it on loopback inside the verifier
container, runs the negative control (`GET /notes/2` with no token must be
`401`), and derives ground truth honestly (log in as `bob`, read bob's own
note). Only then does it parse the agent's artifact as **data** -- `json.load`,
never executed -- and compare the claimed string verbatim.

**Loopback works under `no-network`.** Measured, not assumed: `no-network`
blocks external egress, not `127.0.0.1`. The gate's own HTTP server came up and
answered in a `--network none` container, and in the real Harbor verifier
container (see the pasted output below).

## The two commands

Run from `experiments/harbor-targets/`:

```bash
export PATH="$HOME/.orbstack/bin:$PATH"

# Build, then re-pin: the digest IS the build. Rebuilding changes it.
docker build --platform linux/arm64 -t dawn-mdash-gate:1 mdash/gate
docker inspect dawn-mdash-gate:1 --format '{{index .RepoDigests 0}}'
#   -> paste that exact string into task.toml [verifier.environment].docker_image

harbor run -p mdash -a oracle -o jobs --job-name tc-mdash-pass   # -> 1.0
harbor run -p mdash -a nop    -o jobs --job-name tc-mdash-fail   # -> 0.0
```

Measured: 40s for the oracle trial, 37s for the nop trial.

## Paths, in one table

The gate image and the agent image agree on these by construction, not by
convention -- each is a constant duplicated across the two, so any protocol
that targets this task has to hard-code the same strings. The `/app/outputs`
root itself is not one of them: it is dawn's fixed output directory, defined
once as `outputDir` in `harbor.go`, and this table only restates the name
under it for readers of this target.

| logical name | absolute path | written by | read by |
|---|---|---|---|
| `exploit_result` (the agent's *claim*) | `/app/outputs/exploit_result.json` | agent | gate, as untrusted JSON data; declared in `artifacts` and restored into the verifier container |
| `reward` | `/logs/verifier/reward.json` | gate, unconditionally, as its last act | Harbor (read before `reward.txt`, highest precedence) |
| `finding` (the *published* verdict) | `/logs/verifier/publish/finding.json` | gate, only on a `1` verdict, before `reward.json` | the actuator |
| app source, agent side | `/srv/mdash` | `environment/Dockerfile` | agent |
| app source, gate side | `/opt/mdash` | `gate/Dockerfile` | gate only -- the agent can never see this copy |
| gate entrypoint | `/tests/test.sh` | `gate/Dockerfile` | Harbor |

This table used to point at `/app/exploit_result.json` directly -- the
pre-dawn generation of the contract. A gate reading a path dawn never
delivers an artifact to gets nothing, writes `reward: 0`, and dawn classifies
that as `Rejected`, so the stale path was quietly fabricating rejections;
`gate/selftest.sh` now proves the real path at image build time.

`reward.json` is **numbers only**: `{"reward": 1}`. A string value there raises
ValidationError and fails the whole trial, so the gate never writes one. Nothing
else in this task writes a reward -- there is no `reward.txt`. A gate that dies
before its last line therefore produces no verdict at all, which is an
`infra_error` rather than a silent `0`.

`publish/finding.json` carries the bytes **the gate derived** -- the note id and
the secret it re-read as bob from its own baked copy -- never the agent's
`/app/outputs/exploit_result.json`. The agent's artifact is only ever compared
against; it is never forwarded.

## Real output


```
$ harbor run -p mdash -a oracle -o jobs --job-name tc-mdash-pass
  1/1 Mean: 1.000 ---------------------------------------- 0:00:40 0:00:00
adhoc - oracle
| Trials | Exceptions |  Mean |
|      1 |          0 | 1.000 |
| Reward | Count |
| 1      |     1 |
Total runtime: 40s

$ harbor run -p mdash -a nop -o jobs --job-name tc-mdash-fail
  1/1 Mean: 0.000 ---------------------------------------- 0:00:37 0:00:00
adhoc - nop
| Trials | Exceptions |  Mean |
|      1 |          0 | 0.000 |
| Reward | Count |
| 0      |     1 |
Total runtime: 37s
```

The Reward column reads `1` / `0`, not `1.0` / `0.0`: `reward.json` carries a
JSON int and Harbor prints it back unchanged. The mean is still `1.000`/`0.000`.

The verifier's own stdout, from `jobs/tc-mdash-*/mdash__*/verifier/test-stdout.txt`:

```
=== mdash gate (baked into the digest-pinned verifier image) hostname=8ddda2962b2a ===
-rw-r--r-- 1 501 dialout 54 Sep  4 18:54 /app/exploit_result.json
negative control (no token -> 401): status=401
ground truth re-derived: "bob's secret: the safe combo is 16-23-42"
agent claimed: "bob's secret: the safe combo is 16-23-42"
published /logs/verifier/publish/finding.json
VERDICT: agent produced bob's real note verbatim -> 1
wrote /logs/verifier/reward.json = {"reward": 1}

=== mdash gate (baked into the digest-pinned verifier image) hostname=9eb5ab5e5de4 ===
ls: cannot access '/app/exploit_result.json': No such file or directory
(no artifact at /app/exploit_result.json)
negative control (no token -> 401): status=401
ground truth re-derived: "bob's secret: the safe combo is 16-23-42"
artifact /app/exploit_result.json is absent
agent claimed: None
VERDICT: agent did not produce bob's note -> 0
wrote /logs/verifier/reward.json = {"reward": 0}
```

What Harbor collected back, and what it did not:

```
$ find jobs/tc-mdash-pass -path '*verifier*' -type f
jobs/tc-mdash-pass/mdash__QMMWYMW/verifier/test-stdout.txt
jobs/tc-mdash-pass/mdash__QMMWYMW/verifier/reward.json
jobs/tc-mdash-pass/mdash__QMMWYMW/verifier/publish/finding.json

$ cat jobs/tc-mdash-pass/mdash__QMMWYMW/verifier/publish/finding.json
{"note_id": 2, "secret": "bob's secret: the safe combo is 16-23-42"}

$ find jobs/tc-mdash-fail -path '*verifier*' -type f
jobs/tc-mdash-fail/mdash__aZ8MpYn/verifier/test-stdout.txt
jobs/tc-mdash-fail/mdash__aZ8MpYn/verifier/reward.json
```

No `publish/` on the failing trial: a verdict of `0` publishes nothing, so the
actuator has nothing to file.

And the agent side of the passing trial (`agent/oracle.txt`):

```
mdash toy app listening on 127.0.0.1:8901 fixed=False
logged in as alice
GET /notes/2 with alice's token -> 200 {'note': "bob's secret: the safe combo is 16-23-42"}
wrote /app/exploit_result.json
```

The exploit is real, not a status-code trick: the same `solve.sh` against the
patched app fails, because the flaw is gone.

```
$ docker run --rm --network none -e MDASH_FIXED=1 -v .../solve.sh:/solve.sh:ro dawn-mdash-envtest:1 bash /solve.sh
mdash toy app listening on 127.0.0.1:8901 fixed=True
logged in as alice
GET /notes/2 with alice's token -> 403 {'error': 'forbidden: not your note'}
AssertionError: (403, {'error': 'forbidden: not your note'})
```

## Baking the CLI in (and why the agent phase is no longer `no-network`)

`network_mode = "no-network"` on the **agent** environment does not just block
the task -- it kills Harbor's agent bootstrap before authentication is ever
attempted. `harbor/agents/installed/claude_code.py` reaches
`ensure_system_dependencies(curl, bash, nodejs, npm, procps)` and `apt-get`
exits 100 with no mirror. `--allow-agent-host` does not help: it only applies
during `agent.run()`, not setup.

The fix is one early return. `install()` (:437) bails out when
`_installed_claude_satisfies_version()` passes, and with no version pin that
check is just `_INSTALL_CHECK_COMMAND` (:89):

```
export PATH="$HOME/.local/bin:$PATH"; command -v claude >/dev/null 2>&1
```

So `environment/Dockerfile` now bakes node plus **`@anthropic-ai/claude-code`
pinned to `2.1.259`** (the host's own version; `2.1.260` was latest at build
time -- no `@latest` here). Measured against the built image:

```
$ docker run --rm pb-mdash-envtest:2 sh -lc 'export PATH="$HOME/.local/bin:$PATH"; command -v claude >/dev/null 2>&1; echo rc=$?'
rc=0

$ docker run --rm pb-mdash-envtest:2 claude --version
2.1.259 (Claude Code)

$ docker run --rm pb-mdash-envtest:2 sh -lc 'node --version; npm --version; command -v claude'
v22.23.2
10.9.8
/usr/local/bin/claude
```

`rc=0` is the whole point: Harbor skips both the `apt-get` and the `npm install`.

Node comes from a `node:22-slim` build stage rather than `apt-get`, because
`python:3.13-slim` is trixie and its `nodejs` package would have to be fetched
anyway. `/srv/mdash` is untouched; `solve.sh` still passes inside the new image.

### Agent network policy

```toml
[environment]
network_mode = "allowlist"
allowed_hosts = ["api.anthropic.com", "platform.claude.com"]
```

Legal values are `no-network` | `public` | `allowlist`. **`"none"` is not legal**
and surfaces as the misleading "Either datasets or tasks must be provided."

Both hosts were grepped out of the pinned binary, not guessed:

```
$ docker run --rm pb-mdash-envtest:2 sh -lc "grep -aoE '[A-Za-z0-9:/._-]{0,45}/oauth/token' .../bin/claude.exe | sort -u"
/v1/oauth/token
https://platform.claude.com/v1/oauth/token
```

`api.anthropic.com` (25 occurrences) is the Messages API; `platform.claude.com`
is the OAuth token-refresh endpoint. Deliberately **not** allowed:
`o1158394.ingest.us.sentry.io` (crash telemetry). "statsig" appears in the
binary only as a local cache directory name, never as a host.

The allowlist is really enforced -- measured with a throwaway copy of this task
whose `solve.sh` was replaced by a probe (`pb-netprobe`, scratch dir, not in the
repo):

```
api.anthropic.com        -> HTTP 404 (reachable, TLS ok)
platform.claude.com      -> HTTP 403 (reachable, TLS ok)
example.com              -> BLOCKED: URLError: <urlopen error [SSL: UNEXPECTED_EOF_WHILE_READING] ...>
registry.npmjs.org       -> BLOCKED: URLError: <urlopen error [SSL: UNEXPECTED_EOF_WHILE_READING] ...>
```

The verifier phase is **unchanged**: still `separate` + `no-network` against the
digest-pinned gate image (see "Images" below).

### The gate still discriminates

```
$ harbor run -p mdash -a oracle -o jobs --job-name pb-mdash-pass
  1/1 Mean: 1.000 ---------------------------------------- 0:00:34 0:00:00
| Trials | Exceptions |  Mean |
|      1 |          0 | 1.000 |
Total runtime: 34s

$ harbor run -p mdash -a nop -o jobs --job-name pb-mdash-fail
  1/1 Mean: 0.000 ---------------------------------------- 0:00:36 0:00:00
| Trials | Exceptions |  Mean |
|      1 |          0 | 0.000 |
Total runtime: 36s
```

Verifier stdout from the passing trial is byte-identical to the run recorded
above, modulo the container hostname.

### The cost, stated plainly

| | disk | content (compressed) |
|---|---|---|
| `dawn-mdash-envtest:1` (before) | 211 MB | 46.6 MB |
| `pb-mdash-envtest:2` (after) | 716 MB | 194 MB |
| delta | **+505 MB** | **+147 MB** |

Layer breakdown from `docker history`:

```
219MB  RUN ... npm install -g @anthropic-ai/claude-code@2.1.259
 16MB  COPY /usr/local/lib/node_modules/npm
122MB  COPY /usr/local/bin/node
```

That is not a mistake: the npm package's `install.cjs` downloads a
self-contained 207 MB native binary
(`.../claude-code/bin/claude.exe`, ELF arm64) and `/usr/local/bin/claude` is a
symlink straight to it. Build time is cheap -- **12.7s** with `--no-cache`, once
`node:22-slim` is in the local cache (the npm install itself is 3.0s).

**Node and npm are dead weight at runtime.** Because `claude.exe` is a standalone
ELF, dropping the two `COPY --from=node` layers after the install would save
~138 MB and the CLI would still run. They are kept because the brief asked for
node, and because a missing `node` would be a nasty surprise for any Harbor path
that shells out to it. That is the obvious next cut if image size starts to hurt.

## Images

The verifier image is **digest-pinned** in `task.toml`:

```toml
[verifier.environment]
docker_image = "dawn-mdash-gate@sha256:4af64d4c3652a700563cb91580f6b95008ffddc5eee0c3c0dbd21946a64d270a"
```

OrbStack's image store gives a locally built image a RepoDigest equal to its
image ID, so `docker inspect --format '{{index .RepoDigests 0}}'` yields a
reference Harbor resolves locally with no registry involved. The pin is exact:
change one byte of `gate.py` and the digest changes and the pin must be
updated. That is the point of pinning, not a nuisance.

Provenance that survives a rebuild -- the layer `diff_ids` of the pinned image
(`docker inspect dawn-mdash-gate:1 --format '{{range .RootFS.Layers}}{{println .}}{{end}}'`):

```
sha256:41d6505109809884e681a97f978542a2d4d3506af0124f18b3f3a471edfcc9b7   python:3.13-slim base
sha256:24ee6013411acfda10179bf582a40cc4fc3b2c57372aecc8fb1b6750999fe82e
sha256:a65635ba778221a3d02c961c78b3d53f2a52faa9c95e4c6ba50165bf98db83c1
sha256:7bdec5df92bee072856dee9cb1a7104325355120e9b1fedc032a58dd90bfbe1f
sha256:1a6bd6e8c3fb5dbc4e9cecc2fe26988b08d7be87f1182a2e421afaf2f16c3097
sha256:283e3d8f92cf05596329b61574f0a7ea1ccfb44b9ec8ed9fb7374e9b1e8ff803   COPY app/ -> /opt/mdash
sha256:63d0dca089ca8169df40d885c90669e9f6dc430bb568d9e06bc65fe27242faf9   COPY gate.py + test.sh
sha256:a3aba2a9448bf323921f6b5e2bb439c0dc92dfb44dc9a06ad03e3f1203c28140   chmod + mkdir /app
```

The first six are the unmodified base; only the last two move when the gate
changes.

| Tag | Image ID (digest) | Role |
|---|---|---|
| `dawn-mdash-gate:1` | `sha256:4af64d4c3652a700563cb91580f6b95008ffddc5eee0c3c0dbd21946a64d270a` | pinned verifier; gate baked at `/tests/test.sh`, app at `/opt/mdash` |
| `dawn-mdash-envtest:1` | `sha256:41bc822c287edb589fa5d87ac36edf483c1a2c46405e2ab17e4f0504328fba3e` | local build of `environment/Dockerfile`, used to test `solve.sh` outside Harbor. Harbor builds its own copy of this per run. |
| `pb-mdash-envtest:2` | `sha256:eb2f08eb0ce1c58dcf18f551433af0c959516cf2c589eb3d35ff126048d3f156` | local build of `environment/Dockerfile` **after** baking in Claude Code 2.1.259; the skip-check and `solve.sh` evidence above is from this image. |

Both `linux/arm64`, both `FROM python:3.13-slim`
(`sha256:9d2e5553305c7c7b0097999bb17187c69b921ccd6bc9d40e4bb5ebe652c00285`).

## Layout

```
task.toml                 artifacts = ["/app/outputs"]; separate + no-network
instruction.md            what the agent is told
environment/Dockerfile    agent image; app source -> /srv/mdash
environment/app/*.py      copy of experiments/targets/mdash (docker build context is environment/)
solution/solve.sh         honest oracle: starts the app, logs in as alice, IDORs bob's note
gate/Dockerfile           verifier image
gate/app/*.py             the gate's OWN copy of the app -> /opt/mdash
gate/gate.py              the proving plugin; writes publish/finding.json then reward.json
gate/test.sh              baked at /tests/test.sh
```

## Gotchas hit

- **The docker build context is `environment/`, not the task dir.** `COPY ../..`
  is not possible, so the app source is duplicated into `environment/app/` and
  `gate/app/`. Harbor passes `context_dir=<task>/environment`
  (`harbor/environments/docker/docker.py:243`).
- **`artifacts` must not name a path under `/logs/verifier`** (settled rule 5):
  the artifact is `/app/outputs/exploit_result.json`. Harbor re-materialises it
  at that exact path inside the verifier container before `/tests/test.sh`
  runs -- the `ls -l` line in the pasted verifier stdout is the proof.
- The gate image must `mkdir -p /app`; the artifact restore lands there.
- The target itself needs no egress (stdlib + loopback), but the **agent CLI**
  does -- see "Baking the CLI in" below. `no-network` on the agent environment
  killed Harbor's own agent *setup*, not the task.

## The residual weakness, stated plainly

The gate proves the agent **produced bob's real secret**; it cannot prove the
agent got it *over HTTP*. The app source must be readable in the agent
container for the app to run there, so an agent that just `cat`s
`/srv/mdash/users.py` scores 1.0 without touching the endpoint. Closing that
would need the secret to be unreadable to the agent's uid while readable to the
app's -- a different, heavier design. This task deliberately does not attempt
it; what it isolates is the *transport and gate* question, not shortcut-proof
scoring.

## The codex environment image

`environment/Dockerfile.codex` is the same target for a second vendor: same
base, same `app/`, same `/app/outputs` — differing in exactly one thing, which
agent CLI is on `PATH`. MDASH's defining claim is a panel of *different* models
on the *same* surface, so two images whose sole difference is the vendor of one
binary is the only honest way to express it.

```bash
docker buildx bake mdash-env-codex   # from experiments/harbor-targets/
```

Measured 2026-09-06 (OrbStack, arm64):

| fact | value |
|---|---|
| digest | `dawn-mdash-env-codex@sha256:7003829e738723ee20e50a09849e89cfa94e5ddbd77365a38e58ab6db0c6460c` |
| reproducible | yes — a `--no-cache` rebuild yields the identical digest |
| `command -v codex` | `/usr/local/bin/codex`, exit 0 |
| `codex --version` | `codex-cli 0.145.0` |
| baked tree | 305 MB (`@openai/codex` + its vendored linux-arm64 binary) |

Baked for the same reason claude's is, and the reason is Harbor's ordering, not
convenience: `Codex.install()` returns early only when `_INSTALL_CHECK_COMMAND`
(`command -v codex`) exits 0, and that install runs **before** the agent phase
opens its allowlist. On a `no-network` environment an unbaked CLI cannot reach
the package mirror and the trial dies in setup, before any auth.

Unlike `claude.exe`, which is the binary, codex ships a node wrapper that
dispatches to a vendored native binary — so `node` is load-bearing here, and
the whole package tree comes with it: the vendored codex locates its bundled
`rg` and `zsh` relative to its own path.

**Not yet run against a live model.** Harbor's codex adapter takes ChatGPT
subscription auth via `CODEX_AUTH_JSON_PATH=<a 0600 seed copy>` (never
`CODEX_FORCE_AUTH_JSON`, which reads `~/.codex/auth.json` in place). See
`docs/research/oauth-cli-in-container.md` for why that copy is a landmine
around a refresh: OpenAI burns the old refresh token *before* the write, and
the write lands in a container about to be deleted.
