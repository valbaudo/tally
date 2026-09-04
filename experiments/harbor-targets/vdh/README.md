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

Measured 2026-09-04: **1.000 in 50s** (oracle), **0.000 in 36s** (nop).
Real gate output from `jobs/vdh-pass/vdh__c4rXWLC/verifier/test-stdout.txt`:

```
=== vdh gate (citation/format check only) host=9cd20550a610 ===
findings.jsonl: 2 non-empty line(s)
  line 1: OK   src/users.py:12 class='sql-injection'
  line 2: OK   src/orders.py:14 class='sql-injection'
VERDICT: 1 (well-formed + grounded citations; NOT a correctness oracle)
```

and from `jobs/vdh-fail/vdh__AWpLHwN/verifier/test-stdout.txt`:

```
=== vdh gate (citation/format check only) host=6a15d52ac4e4 ===
FAIL: /app/findings.jsonl does not exist (no findings is a failure, not 'zero findings')
VERDICT: 0 (well-formed + grounded citations; NOT a correctness oracle)
```

## Images (linux/arm64)

| Tag | Image ID |
|---|---|
| `dawn-vdh-gate:1` | `sha256:964f8b6a6a042eb7f8b01801bd34da5ff602d3270e5c462cbf986a319f9e7eae` |
| `dawn-vdh-env:1` (same context Harbor builds from `environment/`) | `sha256:5e755087c551b6b6c59e67eee0d175806c8a1f2a96904cc2c1d9a2bff36b8b0f` |
| base `python:3.13-slim` | `sha256:9d2e5553305c7c7b0097999bb17187c69b921ccd6bc9d40e4bb5ebe652c00285` |

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
- `environment.network_mode = "no-network"` is fine here: the image is built
  before the agent runs, and `python:3.13-slim` was pre-pulled.

## Self-test (host-side, no Harbor)

Six cases were run directly against `dawn-vdh-gate:1`: honest findings → 1;
missing file → 0; empty file → 0; hallucinated evidence → 0; invented
filename → 0; out-of-range line → 0; correctly-cited safe decoy → 1.
