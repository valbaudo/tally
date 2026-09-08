# tally

**tally runs an agent against a verifier and believes only the verifier.**

A *protocol* is a Go program written against package `tally`: it declares stages, bounds
them with leases, reads back a closed set of six states, and — only on `passed` —
actuates. tally compiles each stage into a task it owns entirely, runs it, and assigns a
state **without parsing a single line of agent output**.

Vocabulary is in [CONTEXT.md](CONTEXT.md). Every decision behind the design, with the
measurement that forced it, is on the map: [Map: tally, the mechasuit around any agent
CLI](https://github.com/valbaudo/tally/issues/13).

## Running a protocol from a clean checkout

Four commands. `protocols/prci` is the worked example: an agent is handed a repo whose
CI is red and hands back a unified diff; a separate, pinned, no-network gate applies it
to its **own** pristine repo behind `git apply --include=calc.py`, runs its own baked
suite, and only then does the actuator publish — the bytes the *gate* wrote, never the
agent's.

**1. Host.** [OrbStack](https://orbstack.dev) (or Docker) running, and Harbor:

```bash
uv tool install harbor==0.22.0
```

**2. Build the images.** Every image a protocol pins is built from one declarative file:

```bash
cd experiments/harbor-targets && docker buildx bake -f docker-bake.hcl pr-ci
```

The `cd` is load-bearing: bake resolves each target's `context` against the working
directory, not against the bake file, so running this from the repo root fails with
`unable to prepare context: path "pr-ci/environment" not found`.

The two digests this produces are the constants in
[protocols/prci/main.go](protocols/prci/main.go). **They will match**: the build is a
pure function of this tree, and `docker-bake.hcl` says which seven causes of drift had
to be removed to make that true. If they *don't* match, your source differs from the
commit the pins were taken at — re-pin, don't work around it.

These are **arm64** digests. On amd64 the base images resolve to different bytes, so the
build is reproducible but the digests are not the ones committed here.

**3. Authenticate.** tally spends no money and holds no API key; it inherits a
subscription OAuth token from the environment. Mint one yourself — this step is
deliberately not automated, because it prints a live secret:

```bash
claude setup-token
```

Keep it in a `0600` file **outside this repo**, then:

```bash
export CLAUDE_CODE_OAUTH_TOKEN="$(cat ~/.config/tally-token)"
export CLAUDE_FORCE_OAUTH=1
```

**4. Run.**

```bash
go run ./protocols/prci
```

About 55 seconds. It prints the terminal state and the run directory; the receipt is at
`tally-runs/<run>/report.md`, and every attempt's evidence — the generated task, the
Harbor log, the trial, the collected artifacts — is under `tally-runs/<run>/attempts/`.
Run directories are gitignored.

`TALLY_RESUME=<run-dir> go run ./protocols/prci` re-enters a crashed run: finished
attempts are reconstructed from disk rather than re-dispatched, and actuators that
already fired do not fire again.

`TALLY_MAX_CONCURRENT=<n>` is how wide a fanning agent runs. **Unset means 1**, so a
`Fan` is serial until you say otherwise. tally does not guess this: sizing it from
image bytes and host memory was tried, and it read the image's on-disk size — not a
container's working set — from a `docker image inspect` that runs *before* Harbor
pulls the image, so on any host without that image already cached every fan silently
collapsed to 1 anyway, with nothing to say so. You know your Docker VM's memory and
what one attempt of your image actually costs; tally does not. An agent profile that
cannot fan at all (`codex`) stays capped at 1 regardless of this variable.

### If you skip step 2

The failure is honest but the wording is Docker's, not tally's. Measured, with a gate
image that was never built:

```
Error response from daemon: pull access denied for tally-pr-ci-gate,
repository does not exist or may require 'docker login'
```

tally reports `infra_error` — it obtained no verdict — and the run's `report.md` shows
the attempt with **no metrics and a real token draw**. That is not a wasted diagnostic:
a bad *gate* pin is only discovered **after** the agent has run, so it costs a full
attempt (≈140k tokens in the measured case). A bad *environment* pin fails before the
agent starts.

## What is here

| | |
|---|---|
| `tally.go` | the whole author-facing surface: six states, `Stage`, `Gate`, `Lease`, `Result` |
| `runtime.go` | scopes, leases, admission, fan-out, resume, the actuator |
| `harbor.go` | the runner: generates the task, runs one trial, classifies it |
| `report.go` | the run's receipt |
| `protocols/prci/` | the worked example |
| `experiments/harbor-targets/` | four toy targets and their gates, with the bake file |
| `docs/research/` | primary-source findings the decisions rest on |

⚠️ `docs/salvage/` is a **superseded** design from before a reset on 2026-09-02. It is
kept as a record and is **not authority** — it describes a workflow language and a
`glue` binary that no longer exist.
