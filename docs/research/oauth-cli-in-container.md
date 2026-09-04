# OAuth agent CLIs inside a Harbor container

**Ticket:** dawn #35 · **Date:** 2026-09-04 · **Machine:** this laptop (OrbStack, Harbor 0.22.0, arm64)
**Constraint:** OAuth subscription only. No API keys, no gateway, no spend.

---

## Verdict in one paragraph

Both CLIs authenticate inside a container under an OAuth subscription, and Harbor 0.22.0
has first-class support for both paths — `CLAUDE_FORCE_OAUTH` for claude, `CODEX_AUTH_JSON_PATH` /
`CODEX_FORCE_AUTH_JSON` for codex. **Run the four acceptance protocols on `claude-code`.**
Its credential is a purpose-built, one-year, non-rotating token that Anthropic documents for
exactly this use, it is injected as an env var so N parallel trials cannot corrupt each other,
and the full Harbor→container→api.anthropic.com chain is measured working below. Codex also
works, but its credential is a single-use rotating refresh token shared with the host laptop:
the moment a trial straddles the refresh window, one trial wins and the rest — plus the user's
own `codex login` — are dead. OpenAI's own documentation forbids the fan-out shape dawn wants.
**The four protocols as sketched — alternating claude and codex for cross-vendor decorrelation —
cannot be run as written for the parallel legs.** The honest fallback is claude-code for
everything wide, and codex reserved for single-trial, serialized legs run inside a known-safe
window.

---

## claude — Claude Code 2.1.259

### Verdict: **YES, with one mandatory manual step.**

`claude setup-token` on the host mints a one-year OAuth token bound to the Pro/Max/Team/Enterprise
subscription and prints it to the terminal. It saves nothing. The operator copies it into
`CLAUDE_CODE_OAUTH_TOKEN` at container runtime. No keychain extraction, no credential file moves,
nothing baked into an image. It sits at rank 5 in Claude Code's documented auth precedence, above
the `/login` subscription credential.

**The manual step is not automatable and must not be automated.** `setup-token` prints a live
subscription secret to stdout; an agent running it captures that secret into a transcript. The
user runs it, once, in their own terminal.

### Harbor already implements this path

`harbor/agents/installed/claude_code.py`:

```python
def _should_force_oauth(self) -> bool:
    """Whether to drop the API key so the CLI uses CLAUDE_CODE_OAUTH_TOKEN.

    Opt in via CLAUDE_FORCE_OAUTH=<truthy>; default keeps ANTHROPIC_API_KEY.
    Mirrors codex's CODEX_FORCE_AUTH_JSON.
    """
```

and, in `_resolve_auth_env`:

```python
if force_oauth and not oauth_token:
    raise RuntimeError(
        "CLAUDE_FORCE_OAUTH is set but CLAUDE_CODE_OAUTH_TOKEN is not. "
        "Run `claude setup-token` to get one, or unset CLAUDE_FORCE_OAUTH."
    )
```

Both values reach the agent through `--ae/--agent-env KEY=VALUE` on `harbor run`.

### MEASURED: the chain works end to end inside a real Harbor trial

Run against `experiments/harbor-targets/pr-ci` (the only dawn target whose agent environment is
`network_mode = "public"`), with a **deliberately fake** token — no real credential, no billable
model call:

```
harbor run -p pr-ci -a claude-code -m claude-sonnet-4-5 \
  --ae CLAUDE_FORCE_OAUTH=1 \
  --ae CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-NOT-A-REAL-TOKEN-000000000000
```

Harbor's launch line inside the container (from the trial exception):

```
printf "%s" "$harbor_claude_code_instruction_..." | claude --verbose \
  --output-format=stream-json --permission-mode=bypassPermissions --print \
  2>&1 | tee /logs/agent/claude-code.txt
```

Result, from the stream-json terminal event:

```
"terminal_reason":"api_error", "is_error":true, "api_error_status":401,
"result":"Failed to authenticate. API Error: 401 OAuth access token is invalid."
```

That 401 is the proof. With no credential at all the CLI says `Not logged in · Please run /login`;
with a token present it reads the env var, treats it as the active credential, and carries it to
Anthropic's auth endpoint. No API key was set, no keychain existed in the container, no gateway or
`ANTHROPIC_BASE_URL` was configured. Only a genuine token value is missing, and that is the user's
one manual step.

Two things this also settles:

- **Harbor does not pass `--bare`.** The documented `--bare` blocker ("Bare mode does not read
  `CLAUDE_CODE_OAUTH_TOKEN`") does not apply to Harbor 0.22.0's claude-code adapter. The launch
  line above is the primary evidence. **Pin this.** The docs say `--bare` "will become the default
  for `-p` in a future release"; if Harbor adopts it, this whole path breaks silently into an
  auth error.
- **Runtime injection only.** The token lives in the trial's env, never in an image layer.

### Documented, verbatim

From <https://code.claude.com/docs/en/authentication>:

> "For CI pipelines, scripts, or other environments where interactive browser login isn't
> available, generate a one-year OAuth token with `claude setup-token`"

> "It does not save the token anywhere; copy it and set it as the `CLAUDE_CODE_OAUTH_TOKEN`
> environment variable wherever you want to authenticate"

> "This token authenticates with your Claude subscription and requires a Pro, Max, Team, or
> Enterprise plan. It can only make model requests, so it can't establish Remote Control sessions
> or fetch claude.ai connectors. MCP servers you configure locally still work."

> "Bare mode does not read `CLAUDE_CODE_OAUTH_TOKEN`. If your script passes `--bare`, authenticate
> with `ANTHROPIC_API_KEY` or an `apiKeyHelper` instead."

From <https://code.claude.com/docs/en/devcontainer>:

> "When executed with `--dangerously-skip-permissions`, dev containers do not prevent a malicious
> project from exfiltrating anything accessible inside the container, including the Claude Code
> credentials stored in `~/.claude`."

> "Avoid mounting host secrets such as `~/.ssh` or cloud credential files into the container;
> prefer repository-scoped or short-lived tokens."

That second warning is live for dawn: Harbor runs claude with `--permission-mode=bypassPermissions`
and dawn's targets (mdash, cybergym, vdh) are deliberately hostile code. A trial that executes
untrusted target code with `CLAUDE_CODE_OAUTH_TOKEN` in its environment can exfiltrate the user's
subscription token. dawn's targets are all `no-network` in the environment phase, which is the
mitigation — see the "egress" section below for the tension this creates.

### Real caveats

| Caveat | Consequence |
|---|---|
| One-time manual `setup-token` | Cannot be automated. Human step before any protocol run. |
| Token capped at one year, no documented refresh | Re-run `setup-token` manually at expiry. Nothing rotates mid-run — this is why it is safe to fan out. |
| One token = one seat (Consumer Terms §2) | Fine for the user's own trials on their own laptop. Forecloses sharing with other people or shared CI. |
| Shared usage pool | "usage limits that are shared across Claude and Claude Code". N parallel trials hit rate limits, not scale. Ceiling, not corruption. |
| Bypass-mode exfiltration | Keep targets `no-network` where possible; never point a real token at a target you have not read. |
| `--bare` future default | Pin Harbor's launch line; re-measure after any Harbor upgrade. |

---

## codex — codex-cli 0.145.0

### Verdict: **YES it authenticates, NO it must not be fanned out.**

A ChatGPT-subscription `auth.json` copied into a Linux container authenticates and completes a
real tool-using task end to end — no API key, no keychain, no device binding. Measured: a real
task in `oauth-codex-1` produced `EXIT=0` and a correct file, and `codex login status` returned
`Logged in using ChatGPT`. The container minted its own `installation_id` (different from the
host's) and the token still worked, so nothing is device-, IP-, or fingerprint-bound.

The capability is not the problem. **The credential's lifecycle is.**

### How Harbor injects it

`harbor/agents/installed/codex.py`:

```python
def _resolve_auth_json_path(self) -> Path | None:
    """Resolve which auth.json to inject, if any.

    Defaults to None (OPENAI_API_KEY auth). Opt into auth.json auth via:
      - CODEX_AUTH_JSON_PATH=<path> → use that specific file
      - CODEX_FORCE_AUTH_JSON=<truthy> → use ~/.codex/auth.json
    """
```

```python
await environment.upload_file(auth_json_path, remote_auth_path)
...
setup_command = f'ln -sf {shlex.quote(remote_auth_path)} "$CODEX_HOME/auth.json"\n'
```

Harbor **uploads a copy** into the container and symlinks it at `$CODEX_HOME/auth.json`. The copy
is writable and is destroyed with the container. That is better than a read-only bind-mount — but
it makes the rotation problem *worse*, not better, and it is the crux of this whole section.

### The rotation problem, from source

`codex-rs/login/src/auth/manager.rs` (github.com/openai/codex, main):

```rust
async fn refresh_and_persist_chatgpt_token(&self, auth: &ChatgptAuth, refresh_token: String)
    -> Result<(), RefreshTokenError> {
    let refresh_response = request_chatgpt_token_refresh(refresh_token, auth.client()).await?;
    persist_tokens(auth.storage(), refresh_response.id_token,
                   refresh_response.access_token, refresh_response.refresh_token)
        .map_err(RefreshTokenError::from)?;
    self.reload().await;
    Ok(())
}
```

**The network rotation happens before the write.** OpenAI burns the old refresh token first. And
refresh tokens are single-use — the CLI carries a dedicated classified reason for reuse:

```rust
const REFRESH_TOKEN_REUSED_MESSAGE: &str =
    "Your access token could not be refreshed because your refresh token was already used. \
     Please log out and sign in again.";
const REFRESH_TOKEN_URL: &str = "https://auth.openai.com/oauth/token";
```

Put those two facts together with Harbor's ephemeral copy and the failure mode is exact:

> The first trial that triggers a refresh rotates the token **server-side**, writes the new bundle
> into a container that is about to be deleted, and the new bundle is lost. Every other trial in
> that fan-out, and the user's own `~/.codex/auth.json` on the laptop, now hold a burned refresh
> token. The user is logged out of codex on their own machine.

### When does the refresh fire?

Not on `last_refresh` — the official CI doc is wrong about this. Source:

```rust
const TOKEN_REFRESH_INTERVAL: i64 = 8;
const CHATGPT_ACCESS_TOKEN_REFRESH_WINDOW_MINUTES: i64 = 5;
fn should_refresh_proactively(auth: &CodexAuth) -> bool {
    ...
    if let Some(tokens) = auth_dot_json.tokens.as_ref()
        && let Ok(Some(expires_at)) = parse_jwt_expiration(&tokens.access_token)
    { return expires_at <= Utc::now() + chrono::Duration::minutes(CHATGPT_ACCESS_TOKEN_REFRESH_WINDOW_MINUTES); }
    ...
    last_refresh < Utc::now() - chrono::Duration::days(TOKEN_REFRESH_INTERVAL)
}
```

The trigger is the **access token's JWT `exp`**, five minutes out. `last_refresh` is only the
fallback when the JWT cannot be parsed. Confirmed empirically: `last_refresh` doctored to
2025-01-01 (20 months stale) produced **zero** refresh attempts, because the access token's `exp`
was still in the future.

**This means dawn cannot compute the safe window from outside without parsing the user's JWT.**
The current access token's `exp` was measured at 2026-09-07 — roughly three days from now. Codex
is safe to run today and a landmine next week, and nothing in dawn's config can tell the difference.

### Concurrency: measured safe, structurally unsafe

Two containers ran simultaneously on the identical token: both `EXIT=0`, neither invalidated the
other, and a third run in the original container afterwards still worked. That held **only because
no refresh was due**. It is not evidence that fan-out is safe; it is evidence that fan-out outside
the refresh window is safe, and dawn cannot see the window.

### Other measured facts

- **Schema is strict, all-or-nothing.** Removing `id_token` → ``missing field `id_token` ``, exit 1.
  Removing `refresh_token` → ``missing field `refresh_token` ``, exit 1. `account_id` is optional.
  So dawn **cannot** mount a stripped, access-token-only credential to sidestep rotation — codex
  refuses to parse it.
- **`CODEX_HOME` must be writable and is not small.** Cold start creates `state_5.sqlite`,
  `logs_2.sqlite`, `goals_1.sqlite`, `memories_1.sqlite`, `installation_id`, `config.toml`,
  `models_cache.json`, `sessions/`, `skills/`, `plugins/`, `cache/`. Only `auth.json` itself can
  be read-only.
- **Trace logging leaks PII.** With `RUST_LOG=trace`, `codex_otel.log_only` events print
  `user.account_id=<uuid>` and `user.email=<address>` in plaintext on stdout. If dawn captures
  verbose codex logs into a trial artifact, it ships the subscription owner's email and account id
  with it. (`codex_otel.trace_safe` is the redacted twin.)
- **Exit code is masked by a pipe.** `codex exec ... | tail` reports tail's status: a run that
  failed with ``missing field `refresh_token` `` showed `EXIT=0` through a pipe and `EXIT=1`
  redirected to a file. Redirect, never pipe, or use `PIPESTATUS`.
- **Bubblewrap noise.** `warning: Codex could not find bubblewrap on PATH ... Codex will use the
  bundled bubblewrap in the meantime.` on every containerised run. Harmless; a classifier must
  ignore it.
- **The refresh-under-failure path was never exercised, deliberately.** Forcing it would either
  send the user's real refresh token to OpenAI (risking exactly the logout described above) or
  require faking the clock — and `faketime`/`LD_PRELOAD` is inert against codex, whose vendored
  binary is `ELF 64-bit LSB executable, ARM aarch64, statically linked, stripped` (`ldd` → `not a
  dynamic executable`). The same `faketime` demonstrably worked on node in the same container.
  **The codex failure mode is read from source, not observed.**
- **Host credential untouched.** Baseline `mtime=1787911983 size=3994 mode=600`; identical after
  five containers and every experiment. All work used 0600 copies in a scratchpad, shredded after.

---

## Harbor-level findings (measured here, new)

### 1. dawn's targets are `no-network` in the agent phase — no agent CLI can even install

`harbor run -p mdash -a claude-code ...` failed at **agent setup**, before any auth:

```
NonZeroAgentExitCodeError: Command failed (exit 100):
  apt-get update && apt-get install -y curl bash nodejs npm procps
...
W: Failed to fetch http://deb.debian.org/debian/dists/trixie/InRelease  Connection failed
```

`mdash`, `cybergym` and `vdh` all set `[environment] network_mode = "no-network"`. `--allow-agent-host`
does **not** help: it is "merged into the agent phase allowlist during `agent.run()` only" — setup
runs before that. Only `pr-ci` has `network_mode = "public"`, which is why the probe above ran there.

**Consequence:** to run a real agent on mdash/cybergym/vdh you must either (a) pre-bake node + the
CLI into the environment image, or (b) switch the task to `network_mode = "allowlist"` with
`allowed_hosts` covering the package mirror and the provider. Option (b) weakens the property
mdash was built to demonstrate (a gate that discriminates against an agent with no egress).
**Option (a) is the right one** — it keeps the task's network posture intact and moves the install
cost into a one-time image build.

### 2. `--n-concurrent-agents` is the serialization lever codex needs

`harbor run --n-concurrent-agents <int>`: "Per-agent cap on concurrent agent execution phases; must
be no higher than `--n-concurrent`." Implementation (`harbor/trial/queue.py`) is an
`asyncio.Semaphore` acquired on `AGENT_START` and released on `AGENT_END`, per concurrency pool.
So `--n-concurrent-agents 1` serializes the model-facing phase across a job while environments
still build and verifiers still run in parallel. This is precisely the "serialized job stream"
shape OpenAI's own doc requires — but it converts a 24-wide fan-out into a 24-long queue.

### 3. Harbor already models both OAuth paths

Neither CLI needs a dawn-side hack. `--ae CLAUDE_FORCE_OAUTH=1 --ae CLAUDE_CODE_OAUTH_TOKEN=...`
and `--ae CODEX_AUTH_JSON_PATH=/path/to/seed/auth.json` are the whole integration. Use
`CODEX_AUTH_JSON_PATH` pointed at a dedicated 0600 seed copy, **never** `CODEX_FORCE_AUTH_JSON`,
which reads `~/.codex/auth.json` directly.

---

## Recommendation for the four end-to-end runs

**Use `claude-code` for all four.**

Why, plainly:

1. Its credential does not rotate. Nothing about running N trials in parallel can corrupt it,
   because there is no single-use token to race on. Codex's credential rotates, single-use, and is
   shared with the host laptop's own login.
2. The full chain is measured working inside a real Harbor trial (the 401 above). Codex is measured
   working in a hand-built container, but has **never** been run through a Harbor trial here, and
   its one dangerous path was deliberately not exercised.
3. Anthropic explicitly publishes `setup-token` for this. OpenAI explicitly publishes a rule
   against the shape dawn wants (below).

**What this costs — say it plainly:**

> The four acceptance protocols as sketched alternate between claude and codex to get cross-vendor
> decorrelation. **That cannot be run as written on this laptop.** Codex cannot safely take a
> parallel leg, and cannot safely take *any* leg once its access token enters its five-minute
> refresh window — a moment dawn cannot detect from the outside.

The honest fallback, in order of preference:

- **A. Single-vendor runs, decorrelation declared lost.** All four protocols on claude-code.
  Every cross-vendor claim in the protocol writeups is downgraded to "not measured on this
  machine". This is the recommendation.
- **B. Codex on the narrow legs only.** Where a protocol needs exactly one codex trial and nothing
  runs beside it, run it with `--n-concurrent-agents 1`, from a dedicated seed `auth.json` via
  `CODEX_AUTH_JSON_PATH`, with the user warned that a `codex login` may be required afterwards.
  Do this before 2026-09-07, or re-measure the token's `exp` first. Never as part of a fan-out.
- **C. Not host execution.** Harbor 0.22.0 has no host backend, and the measured 17–25% silent loss
  of concurrent bare-host Claude Code config writes (all exiting 0) makes host fan-out strictly
  worse than the problem it would solve. Do not go here.

**Prerequisite work before any protocol run** (not optional, all four targets need it):

1. User runs `claude setup-token` in their own terminal and holds the value. One-time.
2. Pre-bake node + `@anthropic-ai/claude-code` into the `mdash`, `cybergym` and `vdh` environment
   images so agent setup does not need egress.
3. Give the agent phase egress to `api.anthropic.com` only, via `network_mode = "allowlist"` in
   `[environment]` or `--allow-agent-host api.anthropic.com` at `agent.run()` time. Keep the
   verifier `no-network`.

---

## Concurrency and the 24-wide MDASH fan-out

**On claude-code: it runs.** One env var, 24 containers, no shared mutable credential. The ceiling
is quota, not corruption — "usage limits that are shared across Claude and Claude Code" means 24
concurrent trials draw from one pool and will rate-limit. Expect 429s and stagger with
`--n-concurrent`, but a rate-limited trial fails loudly and retriably; it does not silently poison
the next one. Anthropic is **silent** on whether concurrent multi-agent subscription use is
permitted — treat that as unresolved risk, not documented permission.

**On codex: the 24-wide sketch is not runnable.** 24 trials each get a writable copy of the *same*
single-use refresh token. Outside the refresh window, all 24 succeed (measured at N=2). The first
fan-out that straddles the window has 24 trials racing: one rotates, 23 get a dead credential, and
the host laptop's login dies with them. Serializing with `--n-concurrent-agents 1` makes it safe
and makes it not a fan-out — 24 sequential agent phases, which is a different experiment.

**Concretely: MDASH-24 is a claude-code experiment or it is not an experiment.**

---

## ToS — quoted, not paraphrased

### Anthropic — permitted, by an explicit carve-out

Consumer Terms §3 (<https://www.anthropic.com/legal/consumer-terms>):

> "Except when you are accessing our Services via an Anthropic API Key or where we otherwise
> explicitly permit it, to access the Services through automated or non-human means, whether
> through a bot, script, or otherwise."

Read alone that bars scripted subscription use. It is conditional on two exceptions, and the second
— "where we otherwise explicitly permit it" — is what the Claude Code docs do: `setup-token` is
published "For CI pipelines, scripts, or other environments where interactive browser login isn't
available", and Anthropic's own `claude-code-action` wires that same subscription token into a
GitHub Action ("Pro and Max users can generate this by running `claude setup-token` locally").
**Scripted subscription use via `CLAUDE_CODE_OAUTH_TOKEN` is covered by the carve-out, not
prohibited by it.**

Consumer Terms §2:

> "You may not share your Account login information, Anthropic API key, or Account credentials with
> anyone else or make your Account available to anyone else."

Binds the token to the user personally. dawn on the user's own laptop under the user's own seat is
fine. Distributing that token to other people, a shared runner, or other users of dawn is not.

**Explicitly silent:** <https://support.claude.com/en/articles/11145838> says nothing about
automated, headless, or unattended operation, and nothing about parallel agents or sessions. No
Anthropic document found addresses concurrent multi-agent subscription use in either direction.
Do not infer permission from the `setup-token` carve-out — it covers scripted access and says
nothing about fan-out.

### OpenAI — permitted for CI, explicitly forbidden for fan-out

"Maintain Codex account auth in CI/CD (advanced)", <https://developers.openai.com/codex/auth/ci-cd-auth>:

> "The right way to authenticate automation is with an API key. Use this guide only if you
> specifically need to run the workflow as your Codex account."

> "Treat `~/.codex/auth.json` like a password: it contains access tokens. Don't commit it, paste it
> into tickets, or share it in chat. Do not use this workflow for public or open-source repositories."

> "Use one `auth.json` per runner or per serialized workflow stream."

> "Do not share the same file across concurrent jobs or multiple machines."

> "Do not overwrite a persistent runner's refreshed file from the original seed on every run."

Its own preconditions require "you can preserve the refreshed `auth.json` between runs" and "only
one machine or serialized job stream will use a given `auth.json` copy". **Harbor's ephemeral
per-trial copy violates the first; a fan-out violates the second.**

And <https://developers.openai.com/codex/auth>:

> "Use API key authentication for programmatic Codex CLI workflows, such as CI/CD jobs. Don't
> expose Codex execution in untrusted or public environments."

Europe Terms of Use (EEA resident, updated January 16, 2026), <https://openai.com/policies/terms-of-use/>:

> "You may not share your account credentials or make your account available to anyone else and are
> responsible for all activities that occur under your account."

> "Automatically or programmatically extracting data or Output (defined below)."

> "Interfering with or disrupting our Services, including circumventing any rate limits or
> restrictions or bypassing any protective measures or safety mitigations we put on our Services."

The tension between the last set and the CI guide's explicit blessing of `codex exec` on a ChatGPT
`auth.json` is **not resolved here** — both are quoted as written. What is unambiguous and directly
actionable: OpenAI states in its own Codex documentation that one `auth.json` must not be shared
across concurrent jobs. dawn's fan-out is exactly that shape.

**Neither source is silent.** Anthropic explicitly permits the scripted path and is silent only on
concurrency; OpenAI explicitly permits the CI path and explicitly forbids the concurrent one.

---

## What is genuinely still unknown

1. **No real model call has been made through Harbor with a genuine OAuth token.** The 401 proves
   the wiring; it does not prove a successful completion, a reward, or a trajectory. That step is
   blocked on the user's one-time `claude setup-token`.
2. **Codex has never been run through a Harbor trial here at all.** Everything codex-side is from
   hand-built containers. Harbor's upload-and-symlink path is read from source, not exercised.
3. **The codex refresh-failure mode is source-derived, never observed.** Forcing it means either
   burning the user's real refresh token or faking the clock against a static binary. If dawn ever
   depends on codex, this stays a known unknown.
4. **Rate-limit behaviour at 24-wide is unmeasured.** How many concurrent claude-code trials a Pro
   or Max seat sustains before 429s dominate, and whether Harbor's retry classification handles
   them cleanly, is untested.
5. **Anthropic's position on concurrent multi-agent subscription use is undocumented in either
   direction.** Unresolved risk, not permission.
6. **Whether Harbor's claude-code adapter keeps working after a Harbor or Claude Code upgrade.**
   Two moving pins: the `--bare` default flip, and Harbor's launch line. Re-measure the 401 probe
   after any upgrade of either.
7. **Whether the pre-baked-CLI environment images preserve each gate's soundness.** Adding node and
   an agent CLI to the mdash / cybergym / vdh environment images changes what the agent can reach.
   The gates re-derive ground truth independently, so this *should* be inert — but it is a change
   to a proved artifact and needs a pass/fail re-run of the oracle and nop agents.

---

## Reproduction

```bash
export PATH="$HOME/.orbstack/bin:$PATH"
cd /Users/vabbb/Documents/GitHub/dawn/experiments/harbor-targets

# The fake-token probe. No real credential, no billable call. Expect a 401.
harbor run -p pr-ci -a claude-code -m claude-sonnet-4-5 \
  --ae CLAUDE_FORCE_OAUTH=1 \
  --ae CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-NOT-A-REAL-TOKEN-000000000000 \
  -o /tmp/jobs --job-name oauth-probe -y -q
# then: grep -o 'OAuth access token is invalid' /tmp/jobs/oauth-probe/pr-ci__*/exception.txt

# The no-network wall, on any of the other three targets.
harbor run -p mdash -a claude-code -m claude-sonnet-4-5 ... # -> NonZeroAgentExitCodeError at apt-get
```

Harbor source paths on this machine:

- `/Users/vabbb/.local/share/uv/tools/harbor/lib/python3.14/site-packages/harbor/agents/installed/claude_code.py`
  (`_should_force_oauth`, `_resolve_auth_env`)
- `/Users/vabbb/.local/share/uv/tools/harbor/lib/python3.14/site-packages/harbor/agents/installed/codex.py`
  (`_resolve_auth_json_path`, the `upload_file` + `ln -sf` block)
- `/Users/vabbb/.local/share/uv/tools/harbor/lib/python3.14/site-packages/harbor/trial/queue.py`
  (agent-phase semaphore)

No credential was printed, copied out of the keychain, baked into an image, or sent anywhere. The
host's `~/.codex/auth.json` and `~/.claude/` were not modified. Only `dawn-pg` remains running.
