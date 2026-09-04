# Experiments

Every Harbor trial behind the decisions on [the map](https://github.com/valbaudo/dawn/issues/13), reproducible from a clean checkout.

Findings live in [`../docs/research/`](../docs/research/). This directory holds the inputs that produced them.

## Bring the runtime up

Nothing here is installed by the repo. On a bare machine:

```bash
brew install --cask orbstack && open -a OrbStack
uv tool install harbor
uv tool install "litellm[proxy]==1.99.0" --with prisma --python 3.13
```

Four traps, each of which cost real time:

1. **Pin Python 3.13.** On 3.14 the LiteLLM proxy spins at 100% CPU with an empty log and never listens. `prisma-client-py` 0.15.0 predates 3.14.
2. **`prisma` is not in the `[proxy]` extra**, and its generator exits 0 having silently failed when it is not on PATH:
   ```bash
   cd ~/.local/share/uv/tools/litellm/lib/python3.13/site-packages/litellm/proxy
   PATH="$HOME/.local/share/uv/tools/litellm/bin:$PATH" prisma generate
   ```
3. **Port 5432 may already be taken** by a pre-existing Postgres bound to `127.0.0.1`. OrbStack binds `*:5432` and loses. LiteLLM then reports the misleading `P1010: User 'dawn' was denied access`. Use 55432.
4. **Postgres 15+ locks the `public` schema:** `ALTER SCHEMA public OWNER TO dawn;` before migrations will deploy.

```bash
export PATH="$HOME/.orbstack/bin:$PATH"          # docker is NOT on PATH by default
docker run -d --name dawn-pg -e POSTGRES_PASSWORD=dawn -e POSTGRES_USER=dawn \
  -e POSTGRES_DB=litellm -p 55432:5432 postgres:16
docker exec dawn-pg psql -U dawn -d litellm -c 'ALTER SCHEMA public OWNER TO dawn;'

export DATABASE_URL="postgresql://dawn:dawn@127.0.0.1:55432/litellm"
export LITELLM_MASTER_KEY="sk-dawn-$(openssl rand -hex 16)"   # generate your own; never commit it
PATH="$HOME/.local/share/uv/tools/litellm/bin:$PATH" \
  litellm --config litellm.config.yaml --port 4000 --host 0.0.0.0
```

From inside a container the gateway is `http://host.docker.internal:4000`.

`litellm.config.yaml` routes a `mock-echo` model with a canned reply, so the gateway path is testable with **no provider API key**. There are none on the development machine — both `claude` and `codex` authenticate by OAuth subscription. Anything measuring real spend or caching is still unverified.

## Running a trial

```bash
export PATH="$HOME/.orbstack/bin:$PATH"
harbor run -p <task-dir> -a oracle -o jobs --job-name <unique>
```

`oracle` runs `solution/solve.sh` **inside the agent container**, which makes it a faithful stand-in for an adversarial agent at zero LLM cost. `nop` does nothing. Results land in `jobs/<job-name>/<trial>/` — that directory is generated and deliberately not committed.

Build the verifier images first, from `verifier-image/`:

```bash
cd verifier-image && docker build -t dawn-verifier:1 .    # gate that votes 0
```

## What each task demonstrates

| Task dir | Shows | Result |
|---|---|---|
| `smoke` | the runtime works at all | `1.0` with `oracle`, `0.0` with `nop` — the gate discriminates |
| `preempt` | **the `reward.json` hole**: default (shared) verifier, agent forges a verdict on its way out | **`1.0` on an unsolved task** |
| `preempt-sep` | separate mode with a custom image but the gate *not* baked in | `RewardFileNotFoundError` — Harbor does not inject `/tests` |
| `sep-fresh` | separate mode with no `[verifier.environment]` | `FileNotFoundError` — it looks for an env definition in `tests/` |
| `sep-baked` | separate + pinned + gate baked in, adversarial agent | `0.0` — preemption blocked |
| `sep-good` | same, but the agent **honestly solves the task** | **`0.0` — false negative.** A separate verifier cannot see `/app` |
| `verify-break` | **the forgery that survives separate mode**: `artifacts = ["/logs/verifier/reward.json"]` | **`1.0`** against a gate voting `0` |
| `crash-long` | long-running trial, for interrupting | see crash-cancel findings |
| `crash-timeout` | agent exceeding `[agent] timeout_sec` | `AgentTimeoutError`, verifier still runs |
| `vb1-*` | the reward-precedence and artifact-restore matrix | see the reward-mechanism findings |

`sep-good` and `verify-break` are the two that changed the design: the first shows a separate verifier is blind, the second shows it is still forgeable.

## Gotchas that cost time

- `network_mode = "none"` is **not** legal. The values are `no-network` / `public` / `allowlist`. An illegal value surfaces as `"Either datasets or tasks must be provided."`
- A custom `[verifier.environment] docker_image` does **not** receive `/tests`. Bake the gate into the image.
- `--max-retries` defaults to `0`.
- `SIGKILL` on the `harbor` process **orphans the container and network permanently** — nothing reaps them. Remove them by name.
