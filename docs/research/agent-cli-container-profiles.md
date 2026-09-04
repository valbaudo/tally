# Agent CLI container profiles

Consolidated result of research ticket #17: profile five agent CLIs headless, in a container,
against a LiteLLM gateway (`http://host.docker.internal:4000`, model `mock-echo`, canned reply
`pong from litellm`).

Five independent agents each profiled one CLI in its own `node:22-bookworm` container. This
document merges their findings, flags contradictions, and marks what was not determined. Every
claim below traces to a command run by one of those agents or to Harbor source read on disk;
nothing here is inferred.

Versions profiled: claude 2.1.260 · codex-cli 0.153.2 · droid 0.212.0 · opencode 1.18.27 ·
goose 1.49.0. Host arch aarch64 (OrbStack).

---

## 1. Capabilities

| | **claude** | **codex** | **droid** | **opencode** | **goose** |
|---|---|---|---|---|---|
| Install in `node:22-bookworm` | `npm i -g @anthropic-ai/claude-code` — OK | `npm i -g @openai/codex` — OK | `curl -fsSL https://app.factory.ai/cli \| sh` — OK (needs `curl ca-certificates`) | `npm i -g opencode-ai` — OK | GitHub `download_cli.sh` — OK (needs `curl bzip2 libxcb1 libgomp1`) |
| Version string | `2.1.260 (Claude Code)` | `codex-cli 0.153.2` | `0.212.0` | `1.18.27` | `" 1.49.0"` (leading space, no program name) |
| Binary lands on PATH | yes (`/usr/local/bin`) | yes (`/usr/local/bin`) | **no** — `$HOME/.local/bin`, installer does not edit PATH | yes (`/usr/local/bin`) | **no** — `$HOME/.local/bin`, installer warns and moves on |
| Headless subcommand/flag | `claude -p` / `--print` | `codex exec` | `droid exec` | `opencode run` | `goose run` |
| **Prompt from a file** | yes — `claude -p < prompt.txt` (stdin) | yes — `codex exec ... < prompt.txt`, prints `Reading prompt from stdin...` | yes — `-f/--file prompt.txt`, and stdin | yes — stdin when no positional arg (`-f` is *attachment*, not prompt) | yes — `-i/--instructions FILE`, `-i -` for stdin |
| **Gateway honoured** | yes, Anthropic-native `POST /v1/messages` | **partial** — routed, but LiteLLM 1.99.0 cannot serve it (see §5) | yes, chat/completions | yes, chat/completions — but only via a custom provider | yes, chat/completions |
| How the base URL is set | `ANTHROPIC_BASE_URL` env | **config.toml only** — `OPENAI_BASE_URL` is silently ignored | **no env var exists** — JSON field in `$FACTORY_HOME/.factory/config.json` | `OPENAI_BASE_URL` works for built-in `openai`; a custom provider needs `~/.config/opencode/opencode.json` | `OPENAI_BASE_URL` (bare origin) or `OPENAI_HOST`+`OPENAI_BASE_PATH` |
| Model-name plumbing gotcha | must pin **all five**: `ANTHROPIC_MODEL`, `..._DEFAULT_SONNET/OPUS/HAIKU_MODEL`, `CLAUDE_CODE_SUBAGENT_MODEL` | `wire_api="chat"` **removed** — Responses API only, always streaming | id is `custom:<slug>-<index>`, e.g. `custom:mock-echo-0` | model absent from `provider.*.models` is rejected client-side with a bogus error | none — model name never validated |
| Structured output | `--output-format json` / `stream-json --verbose` | `--json` (JSONL) + `-o FILE` (final text) | `-o json` / `-o stream-json` | `--format=json` (JSON **Lines**) | `--output-format json\|stream-json`, **`-q` mandatory** |
| Completion sentinel to key on | `type=="result"` → `is_error` / `terminal_reason` / `api_error_status` | `turn.completed` vs `turn.failed` | `is_error` / `subtype` | `step_finish` present; scan for `{"type":"error"}` | content block `{"type":"error","kind":...}` — **not** `metadata.status` |
| State root | `/root/.claude/` + `/root/.claude.json` + `/tmp/cc-socks` | `$CODEX_HOME` (default `~/.codex`) | `$HOME/.factory` | `~/.config`, `~/.local/share`, `~/.local/state` (XDG) | `~/.config`, `~/.local/share`, `~/.local/state` (XDG) |
| Isolation knob that works | `CLAUDE_CONFIG_DIR` (**XDG ignored**) | `CODEX_HOME` (**XDG ignored**) + `--ignore-user-config` | `FACTORY_HOME_OVERRIDE` (**XDG unused**) | `XDG_DATA_HOME` / `XDG_STATE_HOME` | `XDG_CONFIG_HOME` / `XDG_DATA_HOME` / `XDG_STATE_HOME` |
| Escapes the isolation knob | `/tmp/cc-socks/<pid>.sock` | nothing outside `$CODEX_HOME` | nothing outside `$FACTORY_HOME` | `/tmp/.<hash>.so` (bun scratch) | nothing outside XDG |
| Harbor adapter | `agents/installed/claude_code.py` | `agents/installed/codex.py` | **ABSENT** | `agents/installed/opencode.py` | `agents/installed/goose.py` |

Verified working launch lines (each produced `pong from litellm`, exit 0):

```sh
# claude — needs the full alias fan-out plus IS_SANDBOX
ANTHROPIC_BASE_URL=http://host.docker.internal:4000 ANTHROPIC_API_KEY=$KEY \
ANTHROPIC_MODEL=mock-echo ANTHROPIC_DEFAULT_SONNET_MODEL=mock-echo \
ANTHROPIC_DEFAULT_OPUS_MODEL=mock-echo ANTHROPIC_DEFAULT_HAIKU_MODEL=mock-echo \
CLAUDE_CODE_SUBAGENT_MODEL=mock-echo IS_SANDBOX=1 CLAUDE_CONFIG_DIR=/task/.cfg \
  claude -p --output-format json --dangerously-skip-permissions < prompt.txt

# codex — config.toml custom provider; NOT end-to-end against LiteLLM 1.99.0, see §5
CODEX_HOME=/work/codexhome OPENAI_API_KEY=$KEY \
  codex exec --skip-git-repo-check -s read-only --json -o /work/last.txt < prompt.txt

# droid — base URL lives in config.json, not the environment
FACTORY_HOME_OVERRIDE=/run/droid FACTORY_AIRGAP_ENABLED=1 \
  droid exec --skip-permissions-unsafe --model custom:mock-echo-0 -o json -f prompt.txt

# opencode — custom @ai-sdk/openai-compatible provider in ~/.config/opencode/opencode.json
LITELLM_BASE_URL=http://host.docker.internal:4000/v1 LITELLM_API_KEY=$KEY \
  opencode --model=litellm/mock-echo run --format=json --auto --dangerously-skip-permissions < prompt.txt

# goose — -q is mandatory or the ASCII banner corrupts stdout JSON
GOOSE_DISABLE_KEYRING=true GOOSE_PROVIDER=openai GOOSE_MODEL=mock-echo \
OPENAI_API_KEY=$KEY OPENAI_BASE_URL=http://host.docker.internal:4000 \
  goose run -q --no-session --output-format json -i prompt.txt
```

---

## 2. Failure signatures

Exit code, then the *identifying* stream. Read the "stderr" column literally: for four of the five
CLIs stderr is empty on model-layer failure.

| Case | claude | codex | droid | opencode | goose |
|---|---|---|---|---|---|
| **bad / invalid credential (401)** | **1** after 10 retries, **~188 s**. stderr empty. stdout json `is_error:true, terminal_reason:"api_error", api_error_status:401` | **1** after 5 retries. stderr: `ERROR: unexpected status 401 Unauthorized: Authentication Error, Invalid proxy server token passed...` (final line printed **twice**) | **1**. stderr `Error during droid execution: Exec failed` (text) / **empty** (json); stdout `subtype:"failure"`. Real 401 only in `logs/droid-log-single.log` | **1**. stderr **empty**; stdout `{"type":"error",...,"statusCode":401,"isRetryable":false}` | **0** ⚠️. stderr **empty**; stdout `error: Ran into this error: Authentication error: ... Status: 401 Unauthorized` |
| **no credential at all** | NOT DETERMINED | **1**, fast, no network: `ERROR: Missing environment variable: OPENAI_API_KEY.` | **1**: `Authentication failed. Please log in using /login or set a valid FACTORY_API_KEY` (Factory-hosted model only; BYOK needs no key) | NOT DETERMINED | **0** ⚠️, stdout `... 401 Unauthorized. Response: Authentication Error, No api key passed in..` |
| **unreachable base URL** | **1** after 10 retries, **~179 s**. stderr empty. stdout `terminal_reason:"api_error"`, **`api_error_status:null`** | **NEVER EXITS** ⚠️⚠️ — `ERROR: Reconnecting... waiting for network` forever, no counter, no ceiling. Exit 124 only from an external `timeout` | **1** after ~7.9 s. stderr empty; stdout `result:"Exec failed"`. Real cause (`Connection error.`) only in the log file | **1**. stderr empty; stdout `{"type":"error",...,"message":"Cannot connect to API: Unable to connect..."`, `isRetryable:true` | **0** ⚠️. stderr empty; stdout `Network error: Could not connect to host.docker.internal:9999 ...` (note: **not** prefixed `error:`) |
| **model rejected 403** | **1**, **fast (291 ms, no retries)**. stdout `api_error_status:403` | **1** after 5 retries: `ERROR: unexpected status 403 Forbidden: Invalid model name passed in model=...` / `turn.failed` | **1**. stderr empty; stdout `result:"Exec failed"`. 403 only in the log file | **1**. stderr empty; stdout `{"type":"error",...,"statusCode":403,"message":"key not allowed to access model..."}` | **0** ⚠️. stdout `error: ... Authentication error: ... Status: 403 Forbidden` (goose classifies 403 as *authentication*) |
| **gateway 429 (budget cap)** | flaps 200/429 (key at ceiling) | **1**: `ERROR: exceeded retry limit, last status: 429 Too Many Requests` — upstream JSON body **discarded** | **1** after 4+ retries stretched over ~90 s of silence | **no exit, no stdout** ⚠️ — retries forever; `timeout 60` → exit 124, **0-byte stdout**. Only trace is `$XDG_DATA_HOME/opencode/log/opencode.log` | not exercised (key was dead) |
| **gateway 500** | NOT DETERMINED | **1**: `ERROR: We're currently experiencing high demand, which may cause temporary errors.` — **misleading**, the 500 is deterministic | NOT DETERMINED | surfaced verbatim in the stdout error event | NOT DETERMINED |
| **SIGTERM mid-run** | **124**, stdout **0 bytes** in `text` and `json`; only `stream-json` yields partials | **124**, stderr empty; JSONL truncated after `turn.started`; `-o` file **not created** | **124**, stdout and stderr **both empty** when killed early — **but see §3** | **124**, stderr empty, stdout truncated to a lone `step_start` (250 B) | **124**, stderr empty, stdout is only the banner |
| **SIGKILL mid-run** | **137**, stdout 0 bytes | **137**, JSONL truncated identically | **137**, both streams empty | **137**, same truncation; leaves a stale lock dir behind | **137**, banner only |
| **bad argv / missing prompt file** | NOT DETERMINED | NOT DETERMINED | **1**, stdout **and** stderr completely **empty** ⚠️ (missing *and* empty prompt file both) | **1**, and the **one** case that writes stderr: `Error: You must provide a message or a command` | **1**, and the **one** case with real stderr: `Instruction file not found — did you mean to use goose run --text?` |
| **malformed config** | NOT DETERMINED | **1**: `Error loading config.toml: wire_api = "chat" is no longer supported.` | NOT DETERMINED | **1**, stderr: `Config file ... is not valid JSON(C):` + `InvalidEscapeCharacter at line 2, column 3` | NOT DETERMINED |

`opencode`'s SIGTERM/SIGKILL/mid-stream-drop rows were produced against a local node stub, not
against LiteLLM; every other opencode row is real gateway traffic. All other CLIs' rows are real
gateway traffic or a local `timeout`.

---

## 3. Exit 0 on failure — the classifier's hit list

This is the section dawn's error-to-state classifier has to encode.

**goose exits 0 on every model-layer failure, comprehensively.**
401 bad key, 401 no key, 403 model rejected, and TCP connection refused **all exit 0**. goose only
exits non-zero for argv/file errors (1) and for signals it did not choose (124/137). A supervisor
that trusts `$?` scores a 100%-failed goose run as a success. Worse, under `--output-format json`
the top-level `metadata.status` reads literally `"completed"` on a run whose only assistant message
is a 401. The only honest signal is a content block with `"type":"error"` and a `"kind"`
(observed: `"authentication"`).

**codex exits 0 with an empty answer.** A well-formed response carrying no output item →
exit **0**, a *Warning* on **stdout** (`Warning: no last agent message; wrote empty content to
/work/e.txt`), a normal `turn.completed` with `output_tokens:0`, and a **zero-byte `-o` file**.
Cheapest correct gate: the `-o` file exists and is non-empty.

**droid lies in the opposite direction — a successful run reports as a timeout.** Non-airgapped
droid prints a complete `{"subtype":"success","is_error":false,"result":"pong from litellm"}` and
then lingers **~6.8 s** flushing telemetry and cloud-sync retries. `timeout 4` → shell exit **124**
with a perfect success document already on stdout. `FACTORY_AIRGAP_ENABLED=1` collapses total
wallclock 7.31 s → 1.17 s and removes the window.

**claude's budget cap is not a cap.** `--max-budget-usd 0.001` on a call costing $0.02305 →
exit **0**, `is_error:false`, `terminal_reason:"completed"`, `total_cost_usd:0.02305`. It overshot
23× and reported clean success; the cap is only checked *between* turns, so any single-turn run
ignores it entirely.

**claude's `subtype` always says `"success"`.** Every hard API failure — 401, 403,
connection-refused — still carries `"subtype":"success"` in the result event. Key off `is_error` /
`terminal_reason` / `api_error_status` and nothing else.

**opencode and codex both emit `error` events on healthy runs.** codex emits
`item.completed{item.type:"error", "Model metadata for \`mock-echo\` not found"}` on *every*
successful run against an unknown model, and Harbor's `openai_base_url` route additionally dumps a
whole raw SSE body into an error frame before succeeding. A naive "any error event ⇒ failure"
check false-positives on every healthy run. Only the terminal `turn.failed` frame means failure.

**And the mirror-image trap: stderr is not a signal, in either direction.**
- A **successful** claude run writes to stderr (`"mock-echo" isn't described by this version's model catalog`, `[claude-code:unrecognized_model]`) while every **failed** claude run writes nothing.
- droid, opencode and goose leave stderr **empty for every model/gateway failure**; all error text rides on stdout as JSON.
- droid goes further: with `-o json`, stderr goes silent even for the text-mode error string. Anything capturing only stderr sees nothing at all.
- codex is the exception — it is the only CLI that reliably puts its error on stderr.

**Two runs never terminate on their own** (not exit-0, but the same class of harm — dawn must own
the wall clock, an idle timeout is not enough):
- codex against an unreachable host: reconnects forever.
- opencode on a retryable error (429): retries forever with **zero bytes** of stdout, byte-for-byte indistinguishable from a hang.
- claude against an unreachable host or bad key takes ~3 minutes to give up. **If dawn's per-trial timeout is under ~200 s, every claude credential failure presents as exit 124 and the diagnostic is lost.**

---

## 4. What droid cost, with no Harbor adapter

Harbor has adapters for the other four. Confirmed absent for droid:
`grep -ril "droid\|factory\.ai\|FACTORY_"` over the whole `harbor/` package returns **nothing**, and
`harbor/models/agent/name.py` has no `DROID`/`FACTORY` member among its 45 agents. (`harbor/agents/factory.py`
is Harbor's own `AgentFactory` registry — unrelated to Factory.ai.) This is the measurement of
"own the launch": five things had to be discovered by hand that an adapter would have carried.

1. **Version pinning has no CLI surface.** The installer hard-codes `VER="0.212.0"` inside the
   shell script; there is no `--version` argument. To pin, dawn must fetch
   `https://downloads.factory.ai/factory-cli/releases/<VER>/<os>/<arch>/droid` directly. Every other
   CLI takes a version in its install command (`npm i -g pkg@<ver>`, or an interpolated release URL).
2. **PATH.** The installer does not edit PATH; `export PATH=$HOME/.local/bin:$PATH` is mandatory.
   (goose shares this problem and prints a warning about it.)
3. **There is no base-URL environment variable.** This is the one thing no existing Harbor adapter
   pattern covers. The endpoint is a JSON field in `$FACTORY_HOME/.factory/config.json`:
   ```json
   {"custom_models":[{"model_display_name":"mock-echo","model":"mock-echo",
     "base_url":"http://host.docker.internal:4000","api_key":"<key>",
     "provider":"generic-chat-completion-api","max_tokens":256}]}
   ```
   `FACTORY_API_BASE_URL` exists and is honoured but points at Factory's own control-plane REST
   API, not a chat endpoint — aiming it at LiteLLM yields `FACTORY_API_KEY is set but appears to be
   invalid`. `droid --settings <path>` does **not** accept `custom_models` (tested: model list came
   back empty). Only `config.json` works.
4. **droid's own error message about that file is wrong.** On an invalid model it prints
   `Note: Custom models are loaded from ~/.factory/settings.json`. They are not — moving the file
   to `settings.json` emptied the model list; moving it back restored it. The file is `config.json`.
5. **Two env vars are load-bearing and undocumented in any adapter.**
   `FACTORY_HOME_OVERRIDE=<dir>` relocates the entire state tree *including where config.json is
   read from* — the only way to configure droid without touching a real `$HOME`.
   `FACTORY_AIRGAP_ENABLED=1` kills all api.factory.ai / telemetry egress (6 retries per run
   otherwise, all 401s) and is what removes the exit-124-on-success window in §3.
6. **Custom model ids are positional:** `custom:<slugified display name>-<index>`. Slugs can
   collide; dawn should read the id back out of the `Custom Models:` block in `droid exec --help`
   rather than constructing it.
7. **All failures collapse to one string.** Bad key, unreachable host, 403 and 429 all surface as
   `result: "Exec failed"`. The HTTP status and provider message exist **only** in
   `$FACTORY_HOME/.factory/logs/droid-log-single.log`. There is no flag that puts the cause on
   stderr, so a droid adapter must ship that log back and grep it to distinguish "gateway down"
   from "model rejected".

Two things came out *cheaper* than expected: `droid exec -f <file>` takes the prompt from a file, so
no `shlex` quoting is needed (Harbor's claude/codex/opencode/goose adapters all wrestle with inline
quoting), and **BYOK needs no Factory account at all** — with a custom model configured, droid runs
end to end with `FACTORY_API_KEY` completely unset. No OAuth, no login. Of the five, droid and goose
are the two easiest to point at an arbitrary gateway.

---

## 5. Gaps and negative results

**codex 0.153.2 + LiteLLM 1.99.0 is not a working pair.** This is the single hard negative result.
codex speaks the Responses API exclusively (`wire_api = "chat"` is now a fatal startup error, not a
fallback) and always with `stream:true`. LiteLLM 1.99.0's `mock-echo` handler cannot stream
`/v1/responses` and returns HTTP 500 —
`'async for' requires an object with __aiter__ method, got ResponsesAPIResponse` — captured off the
wire with an in-container logging proxy. codex maps that 500 to the misleading "We're currently
experiencing high demand". `curl` proves LiteLLM serves the canned text fine on both endpoints
*non-streaming*. The end-to-end `pong from litellm` for codex was obtained only by putting a 30-line
SSE shim in front of the real LiteLLM `/v1/chat/completions` — the text came from LiteLLM, the SSE
framing did not. **Routing is proven; a clean codex→LiteLLM path is not.** Either pin an older
codex, front LiteLLM with a translation shim, or use a gateway that streams Responses.

The **same wall hits opencode's built-in `openai` provider**, independently discovered: it POSTs to
`/v1/responses` and gets the identical 500. Harbor's `_build_register_config_command`
(`opencode.py:470-485`) only injects `baseURL` for `anthropic`/`google`/`openai`, so Harbor+opencode
against a chat/completions-only gateway hits this out of the box. The escape hatch is a custom
`"npm": "@ai-sdk/openai-compatible"` provider, which Harbor can only reach through the
`opencode_config` kwarg.

**No CLI's real tool-use behaviour was exercised. This applies to all five.** `mock-echo` returns a
fixed string and never emits `tool_use` blocks, so no tool ever ran, `permission_denials` was always
`[]`, and no permission-denial exit code was observed. Whether
`--dangerously-skip-permissions` + `IS_SANDBOX=1` (claude), `--skip-permissions-unsafe` (droid),
`--auto` (opencode) or `-s danger-full-access` (codex) actually suffice for unattended tool use is
**NOT DETERMINED** and needs a real model behind the gateway. Related and also **NOT DETERMINED**:
whether any of these CLIs exits 0 while silently failing a real coding task (e.g. refusing a write
in read-only mode but reporting success).

Other **NOT DETERMINED** cells, all marked in §2 rather than guessed: claude's no-credential,
gateway-500 and malformed-config behaviour; codex's and droid's and goose's bad-argv coverage is
partial; goose was never exercised against a 429; droid's and goose's gateway-500 behaviour.

**Install and gateway: no CLI failed to install.** All five installed clean, exit 0, first try. Four
of five reached the real LiteLLM gateway and returned the canned string end to end; codex is the
partial (above).

**Environment caveat that affects reproduction.** The shared virtual key `dawn-smoke` has
`max_budget: 0.1` with `budget_duration: null` — no reset window — and the agents' own system
prompts cost $0.02–0.03 per single-turn call (claude sends ~2095 input tokens, droid ~2.4–3.0 k,
goose ~1015, opencode ~2034 even for a one-word prompt). **Roughly four invocations of any of these
CLIs exhausts the entire shared budget.** Three of the five agents worked around it by minting their
own `models:["mock-echo"]` key with the master key and deleting it afterwards; none modified the
shared key. Raising `max_budget` or minting per-agent keys is a prerequisite for reproducing any of
this.

---

## 6. Contradictions between agents

1. **Reported spend on the shared key is inconsistent, and that is itself the finding.** Within the
   same session the claude agent saw `Current cost: 0.10000000000000003, Max budget: 0.1`, the codex
   agent saw `0.345763`, the goose agent `0.345784`, and the droid agent `0.127378`. The opencode
   agent explains it: LiteLLM's in-memory proxy cost cache had `0.345` while the database showed
   spend `0.0027`. So the 429s are driven by a **stale in-memory cache**, not by true spend, and the
   key flaps between 200 and 429 as the cache diverges. The goose agent concluded "the shared key is
   dead"; the opencode agent concluded "it freed up and I re-ran everything live". Both are true at
   different moments. Do not treat a 429 from this gateway as evidence of actual budget consumption.
2. **`OPENAI_BASE_URL` status is per-CLI, and reads as a contradiction if flattened.** It is
   **ignored** by codex 0.153.2 (which silently goes to api.openai.com and returns a confusing
   OpenAI 401 that looks like a credential problem), **honoured** by opencode's built-in provider,
   and **honoured** by goose. Harbor carries the codex finding as a source comment at
   `codex.py:1403` — verified on disk, though the comment is versioned "codex 0.118.0" while the
   behaviour reproduces on 0.153.2.
3. **No genuine factual contradiction was found between agents on any CLI's own behaviour.** Where
   two agents touched the same subsystem they agreed: codex and opencode independently hit the same
   LiteLLM Responses-streaming 500 with the identical error string, and claude's alias fan-out was
   discovered by hand and then found hard-coded at `claude_code.py:1819-1823`.

---

## 7. Harbor adapter notes (what an adapter carries that a hand launch does not)

Verified on disk at
`/Users/vabbb/.local/share/uv/tools/harbor/lib/python3.14/site-packages/harbor/agents/installed/`.

- **claude_code.py** — `:1819-1823` force-sets the sonnet/opus/haiku/subagent aliases from
  `ANTHROPIC_MODEL` whenever `ANTHROPIC_BASE_URL` is set; `:1830` disables non-essential traffic;
  `:1833` sets `IS_SANDBOX=1`, commented "Allow bypassPermissions mode when running as root inside
  containers"; `:1838` points `CLAUDE_CONFIG_DIR` at the logs dir. Prompt is delivered via an env
  var that is copied to a shell var, `unset`, then `printf`'d into `claude --print` (`:1887-1898`) —
  deliberately never argv. Output: `--verbose --output-format=stream-json --print` teed to a file,
  scanned for `type=="result"`.
- **codex.py** — `:1403` the `OPENAI_BASE_URL` comment; `:1354-1406` writes `openai_base_url` into
  config.toml and `auth.json` into a separate secrets dir; `:1436-1450` delivers the prompt
  **inline**, `shlex.quote`d after `--`, with `</dev/null`. It does **not** parse the `--json`
  stream for the trajectory — it copies `$CODEX_HOME/sessions` out and converts the rollout JSONL.
  Note its route (`openai_base_url` + `auth.json`) first probes `ws://<base>/v1/responses` and dumps
  a raw SSE body into an error frame before falling back; a `[model_providers.X]` custom provider
  makes exactly one POST and is strictly quieter.
- **opencode.py** — `:470-472` "opencode reads baseURL from provider.options, not the provider
  root"; `:505-509` sets `XDG_DATA_HOME`/`XDG_STATE_HOME` into `/logs/agent/...`; `:499,529-532`
  inline `shlex.quote`d prompt with `</dev/null`, `2>&1` merged into the same file it later parses
  as JSON lines (the parser skips `JSONDecodeError` lines at `:164-165`). It **prepends a synthetic
  user step** because opencode's stream omits the user turn, and it **raises
  `NonZeroAgentExitCodeError` from parsed stdout error events rather than trusting the exit code** —
  Harbor already compensating for exactly the problem in §3.
- **goose.py** — `:84-131` installs curl/bzip2/libxcb/libgomp and writes
  `~/.config/goose/config.yaml` *before* install (the CLI will not run without it); `:697-698` sets
  `XDG_DATA_HOME`/`XDG_STATE_HOME`; `:701-728` delivers the prompt as a **recipe YAML** heredoc'd to
  `~/harbor-recipe.yaml`, not `-i` and not `-t`, then `--output-format stream-json 2>&1 | tee`.
  Harbor does not pass `-q`; it survives the banner only because its JSONL parser drops unparseable
  lines (`:380-392`). goose's `stream-json` fragments assistant text into ~3-char chunks sharing one
  `message.id` (`"pon"`, `"g f"`, `"rom"`), so a consumer must aggregate by id — Harbor does this at
  `:395-400`.
- **droid** — no adapter. See §4.
