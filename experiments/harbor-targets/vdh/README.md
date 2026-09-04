# dawn/vdh — toy SQL-injection hunt, gated on citations only

Wraps `experiments/targets/vdh` (five-file Python repo, one bug class seeded
four times by four different splicing mechanisms, each with a safe
bound-parameter sibling as a precision decoy) as a Harbor task.

## What the gate is — and what it is NOT

**The gate is a CITATION / FORMAT check. Passing means "well-formed, grounded
findings". It does NOT mean "correct findings".**

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

`GROUND_TRUTH.md` is **not** baked into either image (verified: `find /
-iname '*GROUND_TRUTH*'` returns 0 hits in both).

## Run it

```bash
export PATH="$HOME/.orbstack/bin:$PATH"
cd experiments/harbor-targets/vdh/gate && docker build -t dawn-vdh-gate:1 .
cd experiments/harbor-targets
harbor run -p vdh -a oracle -o jobs --job-name vdh-pass   # -> 1.0
harbor run -p vdh -a nop    -o jobs --job-name vdh-fail   # -> 0.0
```

Re-measured 2026-09-04 **after baking the CLI in and allowlisting the agent
phase**: **1.000 in 45s** (oracle), **0.000 in 49s** (nop) — unchanged verdicts.
Real gate output from `jobs/pb-vdh-pass/vdh__QKsJPaF/verifier/test-stdout.txt`:

```
=== vdh gate (citation/format check only) host=b5ed17acfd00 ===
findings.jsonl: 2 non-empty line(s)
  line 1: OK   src/users.py:12 class='sql-injection'
  line 2: OK   src/orders.py:14 class='sql-injection'
VERDICT: 1 (well-formed + grounded citations; NOT a correctness oracle)
```

and from `jobs/pb-vdh-fail/vdh__Po6obNQ/verifier/test-stdout.txt`:

```
=== vdh gate (citation/format check only) host=b59000c8f4ec ===
FAIL: /app/findings.jsonl does not exist (no findings is a failure, not 'zero findings')
VERDICT: 0 (well-formed + grounded citations; NOT a correctness oracle)
```

## Images (linux/arm64)

| Tag | Image ID |
|---|---|
| `dawn-vdh-gate:1` | `sha256:964f8b6a6a042eb7f8b01801bd34da5ff602d3270e5c462cbf986a319f9e7eae` |
| agent env (same context Harbor builds from `environment/`) | rebuilt per run; `pb-vdh-env:3` = `sha256:fa8efa4026a2c8775a2048bd23a3e402978487ea057a08dfeeb78bc3f227c826` |
| base `python:3.13-slim` | `sha256:9d2e5553305c7c7b0097999bb17187c69b921ccd6bc9d40e4bb5ebe652c00285` |

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

`GROUND_TRUTH.md` is still absent from both images after the change
(`find / -iname '*GROUND_TRUTH*'` → 0 hits in `pb-vdh-env:3` and in
`dawn-vdh-gate:1`), and `/app/repo` is byte-identical to the pre-change
image (`sha256sum` over every file: no diff).

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
