# dawn/vdh — toy SQL-injection hunt, gated on citations only

Wraps `experiments/targets/vdh` (five-file Python repo, one bug class seeded
four times by four different splicing mechanisms, each with a safe
bound-parameter sibling as a precision decoy) as a Harbor task.

## What the gate is — and what it is NOT

**The gate is a CITATION / FORMAT check. Passing means "well-formed, grounded
findings". It does NOT mean "correct findings".**

This is a `format_only` stage. A `format_only` stage can never reach
`passed`; its 1.0 means *well-formed*, not *correct*. Do not "improve" this
gate into an oracle — that would break a settled decision, not fix a
limitation.

There is deliberately no sound oracle for this target. The gate has never
seen `GROUND_TRUTH.md` and cannot tell a true positive from a decoy. It
checks only that the agent produced findings it can actually point at:

1. `/app/findings.jsonl` exists and has at least one non-empty line —
   **absence is failure**, not "the agent found zero bugs".
2. Every line is a JSON object with `file`, `line`, `class`, `evidence`.
3. `file` resolves to one of the five real files in the repo, and `line` is
   in range for that file **in the source baked into the gate image**.
4. The cited line is non-trivial (≥8 chars stripped, not a `#` comment).
5. `evidence` is a **verbatim** snippet of that exact line, whitespace
   normalized. This is the anti-hallucination check: an invented line
   number or a paraphrased quote is rejected.

Measured proof it is not an oracle: a correctly-cited **safe decoy**
(`src/users.py:6`, a bound-parameter query) scores **1.0**. Recall and
precision against `GROUND_TRUTH.md` are for a human to compute host-side.

`GROUND_TRUTH.md` is **not** baked into either image (verified: `find / -xdev
-iname '*GROUND_TRUTH*'` returns 0 hits in both).

## The verdict file

`gate/check.py` writes **`/logs/verifier/reward.json`, unconditionally, as
its last act** (`gate/check.py:83`), and nothing else in this task writes a
reward:

```python
pathlib.Path("/logs/verifier").mkdir(parents=True, exist_ok=True)
pathlib.Path("/logs/verifier/reward.json").write_text(json.dumps({"reward": reward}))
```

- **`reward.json`, not `reward.txt`.** Harbor reads `reward.json` first; it
  is the highest-precedence verdict file. Harbor restores declared
  `artifacts` into the verifier container *before* `/tests/test.sh` runs, so
  writing the verdict **last** is what makes it unforgeable: whatever the
  agent may have staged, the gate's write is the one that lands.
- **Numbers only.** `{"reward": 1}`. A string value raises ValidationError
  and fails the whole trial. Extra *numeric* keys are allowed and surface as
  extra columns; this gate emits none.
- **No `reward.txt` fallback anywhere** — the old write at `check.py:79` is
  gone, and the unused task-dir `tests/test.sh` was moved to `reward.json`
  too. A gate that dies before its last line writes nothing, and Harbor
  reports that as `infra_error` rather than as a 0.0 verdict. That is the
  point: a crash is not a judgement.

No actuator publishes from this target, so the gate writes nothing to
`/logs/verifier/publish/`.

## Output paths

The one path the agent writes and the gate reads is a constant duplicated
between `instruction.md`, `task.toml`'s `artifacts`, and `gate/check.py`.
Any protocol targeting this task needs this mapping:

| Logical output | Absolute path (agent container) | Declared in |
|---|---|---|
| `findings` | `/app/findings.jsonl` | `task.toml` `artifacts`, `gate/check.py:10` |
| `repo` (read-only input, agent's copy) | `/app/repo` | `environment/Dockerfile` |
| `repo` (gate's own re-derived copy) | `/gate/repo` | `gate/Dockerfile`, `gate/check.py:11` |
| verdict (gate-written, never agent-visible) | `/logs/verifier/reward.json` | `gate/check.py:83` |

`/gate/repo` is the gate's private copy; the agent can neither read nor
write it, and the gate never trusts `/app/repo`.

## Run it

```bash
export PATH="$HOME/.orbstack/bin:$PATH"
cd experiments/harbor-targets/vdh/gate && docker build -t dawn-vdh-gate:1 .
# rebuilding changes the digest -- re-pin task.toml from:
docker inspect dawn-vdh-gate:1 --format '{{index .RepoDigests 0}}'
cd experiments/harbor-targets
harbor run -p vdh -a oracle -o jobs --job-name tc-vdh-pass   # -> 1.0
harbor run -p vdh -a nop    -o jobs --job-name tc-vdh-fail   # -> 0.0
```

Re-measured 2026-09-04 **after moving to `reward.json` and digest-pinning
the gate image**: **1.000 in 49s** (oracle), **0.000 in 43s** (nop) —
verdicts unchanged, so the gate still discriminates.

`jobs/tc-vdh-pass/vdh__iUcEEBc/verifier/`:

```
=== vdh gate (citation/format check only) host=377b470959c1 ===
findings.jsonl: 2 non-empty line(s)
  line 1: OK   src/users.py:12 class='sql-injection'
  line 2: OK   src/orders.py:14 class='sql-injection'
VERDICT: 1 (well-formed + grounded citations; NOT a correctness oracle)
```
```
$ cat jobs/tc-vdh-pass/vdh__iUcEEBc/verifier/reward.json
{"reward": 1}
```

`jobs/tc-vdh-fail/vdh__u5Hhaz8/verifier/`:

```
=== vdh gate (citation/format check only) host=7ada280f5533 ===
FAIL: /app/findings.jsonl does not exist (no findings is a failure, not 'zero findings')
VERDICT: 0 (well-formed + grounded citations; NOT a correctness oracle)
```
```
$ cat jobs/tc-vdh-fail/vdh__u5Hhaz8/verifier/reward.json
{"reward": 0}
```

Both verifier directories contain `reward.json` and `test-stdout.txt` and
**no `reward.txt`**.

## Images (linux/arm64) — the gate is digest-pinned

`task.toml` names the verifier image by **digest**, not by tag:

```toml
[verifier.environment]
docker_image = "dawn-vdh-gate@sha256:87ad8b9b5d39f4dd0fe0eee58b9f6f2902c9d3a55eb60676796ffc70e7a66db0"
```

OrbStack's image store gives locally-built images a RepoDigest equal to the
image ID, so no registry push is needed:

```
$ docker inspect dawn-vdh-gate:1 --format '{{index .RepoDigests 0}}'
dawn-vdh-gate@sha256:87ad8b9b5d39f4dd0fe0eee58b9f6f2902c9d3a55eb60676796ffc70e7a66db0
$ docker inspect dawn-vdh-gate:1 --format '{{.Id}}'
sha256:87ad8b9b5d39f4dd0fe0eee58b9f6f2902c9d3a55eb60676796ffc70e7a66db0
```

**Rebuilding changes the digest — that is what pinning means.** Re-run the
`RepoDigests` inspect and update `task.toml` after every gate rebuild.

The pin is load-bearing, not decorative. Measured: swapping the digest for
`sha256:0000…0000` and running `harbor run -p vdh -a nop` gives
**0 trials, 1 exception (`RuntimeError`)** in 28s — an infra failure, not a
0.0 verdict. Restored afterwards.

Provenance that survives a rebuild — the layer `diff_ids` of the gate image
(`docker inspect dawn-vdh-gate:1 --format '{{range .RootFS.Layers}}{{println .}}{{end}}'`):

```
sha256:41d6505109809884e681a97f978542a2d4d3506af0124f18b3f3a471edfcc9b7
sha256:24ee6013411acfda10179bf582a40cc4fc3b2c57372aecc8fb1b6750999fe82e
sha256:a65635ba778221a3d02c961c78b3d53f2a52faa9c95e4c6ba50165bf98db83c1
sha256:7bdec5df92bee072856dee9cb1a7104325355120e9b1fedc032a58dd90bfbe1f
sha256:80974fd24c795ef09bced1e4930dae1b4dd40364b615289b0fcda447151db9df
sha256:492ede23dfa7e7cfb1bed3f783bdc14153e7782de88c57aaf5cd549e666fb650
sha256:71ca7c952a26fa14869f69f2e51fea9dbf1407730d44824141db888020941837
sha256:9951e203ffba4e1494d409d6c9f353cdffc40b0074b5da1a887db33b862e87e3
```

| Tag | Image ID |
|---|---|
| `dawn-vdh-gate:1` (pinned) | `sha256:87ad8b9b5d39f4dd0fe0eee58b9f6f2902c9d3a55eb60676796ffc70e7a66db0` |
| agent env (same context Harbor builds from `environment/`) | rebuilt per run; `pb-vdh-env:3` = `sha256:fa8efa4026a2c8775a2048bd23a3e402978487ea057a08dfeeb78bc3f227c826` |
| base `python:3.13-slim` | `sha256:9d2e5553305c7c7b0097999bb17187c69b921ccd6bc9d40e4bb5ebe652c00285` |

The agent image is **not** pinned: Harbor rebuilds it from `environment/`
on every run, so there is no stable digest to pin. Only the gate — the
thing that decides the verdict — needs to be immutable.

## Agent CLI is baked in; agent phase is allowlisted

`environment/Dockerfile` bakes **Claude Code `2.1.259`**, pinned, into the
agent image. Why it has to be baked rather than installed at runtime:

- Harbor's `ClaudeCode.install()` (`harbor/agents/installed/claude_code.py:437`)
  returns early when `_installed_claude_satisfies_version()` passes. With no
  version pin on the Harbor side that check is just `_INSTALL_CHECK_COMMAND`
  (`:89`): `export PATH="$HOME/.local/bin:$PATH"; command -v claude >/dev/null 2>&1`.
- `ensure_system_dependencies(curl, bash, nodejs, npm, procps)` is only
  reached on the **non**-skip path. Under `no-network` that `apt-get` exits
  100 and the trial dies before authentication is ever attempted.
- Agent *setup* runs under the `[environment]` baseline, **not** the agent
  phase policy — `resolve_agent_phase_policy()` is documented as "effective
  agent policy during `agent.run()`". So an allowlist alone would not have
  saved setup. Baking is the fix; the allowlist is only for `run()`.

Measured on the built image:

```
$ docker run --rm pb-vdh-env:3 sh -lc 'export PATH="$HOME/.local/bin:$PATH"; command -v claude >/dev/null 2>&1; echo rc=$?'
rc=0
$ docker run --rm pb-vdh-env:3 sh -lc 'command -v claude; claude --version'
/usr/local/bin/claude
2.1.259 (Claude Code)
```

Also verified `rc=0` and `claude --version` with `--network none` **and**
`--user 1000:1000`, i.e. under the conditions agent setup actually sees.

Claude Code 2.1.259 ships as a self-contained native ELF
(`bin/claude.exe`, 216 MB), so node is used only in a build stage to resolve
the pin — the runtime image has **no node and no npm** (`command -v node` →
127). Copying the single binary instead of the whole `node_modules` tree
saved 190 MB (716 MB → 526 MB) versus keeping node around.

Network policy, resolved by Harbor's own resolver against this `task.toml`:

```
agent_env_baseline    = no-network   allowed_hosts=[]
agent_phase           = allowlist    allowed_hosts=['api.anthropic.com', 'console.anthropic.com', 'statsig.anthropic.com']
verifier_env_baseline = no-network   allowed_hosts=[]
verifier_phase        = no-network   allowed_hosts=[]
```

The verifier stays `no-network` on both baseline and phase, on its pinned
baked `dawn-vdh-gate:1`. Legal `network_mode` values are
`no-network` | `public` | `allowlist`; `"none"` is **not** legal and fails
with the misleading "Either datasets or tasks must be provided."

The allowlist is not just config: a trial now brings up Harbor's egress
sidecar alongside the agent container, which a pure `no-network` task does
not need. Caught mid-run with `docker ps`:

```
vdh__navumar__env-main-1                                    vdh__navumar__env-main   Created
vdh__navumar__env-harbor-docker-egress-control-sidecar-1    harbor-prebuilt:...      Up (health: starting)
```

`docker inspect` on it shows `EGRESS_CONTROL_INITIAL_NETWORK_MODE=no-network`
with an empty `EGRESS_CONTROL_INITIAL_ALLOWED_HOSTS` — i.e. it boots at the
`[environment]` baseline and Harbor flips it to the allowlist for
`agent.run()`. That is exactly the dynamic phase switch
`Trial._validate_dynamic_phase_switch()` pre-validates.

The three allowlisted hosts are Anthropic's documented Claude Code egress
set (API, auth/token refresh, feature flags). **Only the fact that the
allowlist config resolves, the sidecar attaches, and the trial runs is
measured** — end-to-end authenticated traffic was not tested here,
deliberately: no credential was used, and `oracle`/`nop` need none. Whether
`console.anthropic.com` / `statsig.anthropic.com` are genuinely required is
reasoned, not measured; trim them if a real run shows they are not.

`GROUND_TRUTH.md` is still absent from both images, re-verified after the
`reward.json` + digest-pin change:

```
$ docker run --rm pb-vdh-env:3 sh -c 'find / -xdev -iname "*GROUND_TRUTH*"'
$ docker run --rm dawn-vdh-gate@sha256:87ad8b9b5d39f4dd0fe0eee58b9f6f2902c9d3a55eb60676796ffc70e7a66db0 \
    sh -c 'find / -xdev -iname "*GROUND_TRUTH*"'
```

Both print nothing — 0 hits. `environment/` was not touched by this change,
so the agent image is byte-for-byte the one measured above.

## Transport

`artifacts = ["/app/findings.jsonl"]`. The separate verifier cannot see the
agent's filesystem; Harbor re-materialises declared artifacts at their
original absolute path inside the verifier container before `/tests/test.sh`
runs. The path is under `/app`, **never** under `/logs/verifier` — that
would let the agent forge the verdict.

The gate re-derives what it can: the repo is baked into the gate image at
`/gate/repo`, and the agent's jsonl is parsed as data only. Nothing the
agent writes is ever executed.

## Gotchas hit

- Harbor's docker build context is `environment/` (and, for the gate,
  `gate/`), not the task root — so the vdh source is vendored into **both**
  contexts. They are byte-identical to `experiments/targets/vdh/src`
  (`diff -r` clean); if the shared source ever moves a line, re-copy both or
  the gate's line-range check drifts from what the agent sees.
- `upload_artifacts` is best-effort: a missing artifact is silently skipped
  rather than raising. That is what makes the `nop` path reach the gate and
  score 0 instead of erroring out — but it also means a gate that treats a
  missing artifact as "vacuously fine" would hand out free 1.0s. Hence
  check 1 above.
- `environment.network_mode = "no-network"` is the **baseline** and stays
  that way; the agent gets network only via the `[agent]` phase override.
  Image builds happen before any of this and are unaffected.
- Image cost is real and not small: **211 MB → 526 MB (+315 MB)**, almost all
  of it the 216 MB Claude Code binary. Build is cheap though — 12s cold
  (`--no-cache`) with base images already pulled, 10s warm.
- `instruction.md` tells the agent to run `python3 /app/repo/demo_exploits.py`,
  which fails with `ModuleNotFoundError: No module named 'db'` because that
  script does `sys.path.insert(0, "src")` — a *relative* path, so it only
  works from `cwd=/app/repo`. Pre-existing, identical in the pre-change
  image, left alone: the seeded source is vendored byte-identical into both
  build contexts and touching it would drift the gate's line-range check.

## Self-test (host-side, no Harbor)

Six cases were run directly against `dawn-vdh-gate:1`: honest findings → 1;
missing file → 0; empty file → 0; hallucinated evidence → 0; invented
filename → 0; out-of-range line → 0; correctly-cited safe decoy → 1.
