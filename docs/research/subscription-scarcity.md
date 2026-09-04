# Subscription scarcity — what's actually scarce, and what reports it

dawn tests exclusively against OAuth-subscription Claude Code (no API keys, no gateway), so
dollar cost is notional, not a real constraint. This asks what the real constraint is, and
whether Harbor's normalized channel — the only thing dawn reads, since dawn parses no per-CLI
output — can see it.

- **Harbor version:** 0.22.0, source read from
  `/Users/vabbb/.local/share/uv/tools/harbor/lib/python3.14/site-packages/harbor/`
- **Method:** every claim below is either a direct source-code citation (`file:line`) or a
  quote from a fetched Anthropic doc (URL given, raw HTML pulled with `curl` and stripped of
  markup — not paraphrased by a search summarizer). No new model calls were made; no quota was
  touched. Builds on `docs/research/oauth-cli-in-container.md` (one OAuth token = one seat,
  shared usage pool) and `docs/research/harbor-crash-cancel-behaviour.md` (confirms
  `exception_info.exception_type` is a real, populated field with distinct values per failure
  mode) without redoing either.

---

## 1. Per-adapter completeness of the normalized channel

`AgentContext` (`harbor/models/agent/context.py:8-20`) declares four numeric fields —
`n_input_tokens`, `n_cache_tokens`, `n_output_tokens`, `cost_usd` — each `int | float | None`.
Every installed agent populates them itself in `populate_context_post_run`; there is no shared
computation. All four adapters below use the same overall shape (parse the CLI's own
session/stream log → build a `FinalMetrics` → copy four numbers onto `context`), but the
*source* of each number, and what's silently folded together, differs per adapter.

| Field | claude_code.py | codex.py | goose.py | opencode.py |
|---|---|---|---|---|
| `n_input_tokens` | `input_tokens + cache_read_input_tokens + cache_creation_input_tokens`, summed per turn from the session JSONL's Anthropic `usage` blocks (`claude_code.py:854-858`, `1531`, `1625`). Cache creation is folded in here. | Codex's own **cumulative** `total_token_usage.input_tokens` from the last `token_count` event — Harbor trusts Codex's running total rather than re-summing (`codex.py:1105-1109`, `1195`). | `input_tokens` (goose ≥1.37) or `total_tokens` (older goose, combined counter) from goose's `complete` event; goose's own comment says this already includes cache reads+writes (`goose.py:583-589`, `556-560`, `640`). | `tokens.input + cache.read`, summed per turn from OpenCode's `step_finish` events (`opencode.py:305-313`, `384`, `433`). |
| `n_cache_tokens` | `cache_read_input_tokens` only — cache **creation** tokens are inside `n_input_tokens` above but never broken out here (`claude_code.py:854`, `1526-1533`, `1626`). | `total_token_usage.cached_input_tokens` (reads only); `cache_write_input_tokens` exists in the payload but only reaches `extra`, never a normalized field (`codex.py:527-528`, `1112-1132`, `1196`). | `cache_read_input_tokens`, **only on goose >1.43** (block/goose#10430); on older goose stays `None` (not 0) by explicit design so downstream shows "unknown," not "zero" (`goose.py:585-587`, `642-645`). | `cache.read` only; `cache.write` lives in per-step `extra.cache_write_tokens`, never summed into a normalized field (`opencode.py:308-332`, `386`, `435`). |
| `n_output_tokens` | Summed `output_tokens` per turn (`claude_code.py:859`, `1627`). | `total_token_usage.output_tokens`, Codex's own cumulative counter (`codex.py:1110`, `1197`). | `output_tokens` (goose ≥1.37); `None` on older goose, since only a combined total exists then (`goose.py:592`, `641`). | Summed `tokens.output` per turn (`opencode.py:306`, `385`, `434`). |
| `cost_usd` | **Authoritative:** Claude Code's own `{"type":"result","total_cost_usd":...}` line from `--output-format=stream-json`, parsed from the teed stdout log (`claude_code.py:956-985`). **Fallback:** per-step `litellm.cost_per_token()` estimate, tagged `extra.cost_source="litellm_estimate"` (`claude_code.py:987-1041`). `None` if both fail. | Comment states plainly: "Codex CLI does not include cost in token_count events" (`codex.py:1116-1117`) — `info.get("total_cost")` / `info.get("cost_usd")` are dead-letter fallbacks that in practice never fire; cost is **always** an estimate: per-API-call `litellm.cost_per_token()`, summed (`codex.py:724-781`, `1083-1090`). One unpriced call anywhere in the trial (model absent from litellm's table) zeroes the *entire* trial's cost to `None` (`codex.py:1086-1088`). | `usage.cost_usd`, **goose's own self-reported figure** from its `complete` event (not litellm-derived) — only present on goose >1.43; else `None`, never defaulted to 0 (`goose.py:597`, `646-647`). | Sum of per-turn `finish.cost`, OpenCode's own self-reported figure; `total_cost if total_cost else None` — an all-zero session (e.g. an unpriced/subscription auth path) correctly yields `None`, not a misleading `$0.00` (`opencode.py:304`, `312`, `323`, `387`, `432`). |

**Takeaways for what dawn can honestly report:**
- **All four are internally consistent but not cross-comparable.** claude_code.py folds cache
  *creation* into `n_input_tokens`; codex.py and opencode.py never surface cache *writes* in any
  normalized field at all (buried in `extra`, which dawn doesn't read). Summing `n_input_tokens`
  across a mixed-agent fleet is not an apples-to-apples token count.
- **`cost_usd` provenance is agent-specific and not always labeled.** Only claude_code.py marks
  a `cost_source="litellm_estimate"` fallback in its trajectory `extra` (still inside
  `agent_result`'s sibling trajectory file, not in the four `AgentContext` numbers dawn reads).
  For codex.py the number is *always* a litellm estimate in practice, silently. Goose and
  OpenCode report a number the CLI computed itself, of unknown internal provenance.
  **NOT DETERMINED:** whether goose's/OpenCode's self-reported `cost_usd` is zeroed or garbage
  under OAuth/subscription auth (no ANTHROPIC_API_KEY) — not tested, since that requires a live
  run.
- **None-vs-zero is handled carefully in goose.py and opencode.py** (explicit comments guarding
  against a misleading `0`), less so in codex.py (one unpriced call nukes the whole cost figure)
  and claude_code.py (`or 0` on the three token fields, so a `None`-vs-legitimate-zero
  distinction is lost for tokens, though not for cost).
- **Roughly 20 other adapters were confirmed present** (`harbor/agents/installed/*.py`) but
  **not read in this pass** — the ticket named these four. `harbor/agents/computer_1/` and
  `harbor/agents/terminus_2/` use a structurally different, non-`installed/` code path
  (`context.n_input_tokens = chat.total_input_tokens`, no `Metrics`/`FinalMetrics`
  intermediary) — **NOT DETERMINED** in detail, flagged only as evidence the pattern isn't
  universal.
- `TrialResult.compute_token_cost_totals()` (`harbor/models/trial/result.py:96-134`) sums these
  four fields across either the single `agent_result` or all `step_results[i].agent_result` —
  confirming the channel really is agent-agnostic at the point dawn would read it.

---

## 2. What's genuinely scarce, from Anthropic's own docs

All quotes below were pulled by fetching the raw page and stripping markup myself (not a search
engine's paraphrase).

**Two limits, not one, both usage- not dollar-denominated:**

> "Your session-based usage limit will reset every five hours. Max plans also have a weekly
> usage limit that applies across all models. The weekly limit resets at a fixed time each week
> that is assigned to your account. Your reset day and time stay the same regardless of when you
> start using Claude or when your subscription begins, and you receive your full weekly
> allowance each cycle."
> — [What is the Max plan?](https://support.claude.com/en/articles/11049741-what-is-the-max-plan)

**A per-model sub-limit exists at the weekly layer** — Opus is metered separately from
everything else:

> "**Weekly limits:** Check when your plan's weekly usage limit resets for Opus only and all
> other models."
> — [Usage limit best practices](https://support.claude.com/en/articles/9797557-usage-limit-best-practices)

**The pool is shared across every surface**, not just the CLI:

> "Both Pro and Max plans offer usage limits that are shared across Claude and Claude Code,
> meaning all activity in both tools counts against the same usage limits."
> — [Use Claude Code with your Pro or Max plan](https://support.claude.com/en/articles/11145838-use-claude-code-with-your-pro-or-max-plan)

> "Your usage of all different Claude product surfaces (claude.ai, Claude Code, Claude Desktop)
> counts towards the same usage limit."
> — [How do usage and length limits work?](https://support.claude.com/en/articles/11647753-how-do-usage-and-length-limits-work)

**No published numbers.** Every doc above describes the *mechanism* (five-hour rolling window,
fixed weekly reset, Opus split out) but none states a token or message count — Anthropic
explicitly reserves discretion beyond even that:

> "In addition, to manage capacity and ensure fair access to all users, we may limit your usage
> in other ways, such as weekly and monthly caps or model and feature usage, at our discretion."
> — same Max-plan article.

**Concurrency / parallel agents on one seat: NOT DOCUMENTED.** Anthropic's official Claude Code
docs describe *how* to run parallel sessions
([Run parallel sessions with worktrees](https://code.claude.com/docs/en/worktrees), shipped
v2.1.49) with zero mention of usage-pool interaction — no cap on concurrent processes, no
statement that N sessions drain the shared pool N× faster (which follows logically from "all
activity... counts against the same usage limits," but is never stated as a concurrency rule).
This matches and does not contradict the already-established finding in
`docs/research/oauth-cli-in-container.md`: "Shared usage pool ... N parallel trials hit rate
limits, not scale. Ceiling, not corruption."

**What a user sees (exact strings), and what a *process* sees (HTTP/JSON):**

The consumer-facing (claude.ai / Claude Code UI) messages, verbatim:

> "Usage limit warnings appear when you're approaching your plan's limit within a five-hour
> session: 'Approaching 5-hour limit.' If you hit your plan's limit after the warning appears,
> you'll see a blocking error message letting you know when you can use Claude again: '5-hour
> limit reached - resets [time].'" ... "Paid Claude users with usage credits enabled ... will
> see ... '5-hour limit resets [time] - continuing with usage credits.'"
> — [Troubleshoot Claude error messages](https://support.claude.com/en/articles/12466728-troubleshoot-claude-error-messages)

The Claude-Code-specific (Enterprise-seat) phrasing is worded slightly differently in its own
doc:

> "Claude Enterprise seat (via `/login`) → A pool of usage included in your organization's plan,
> reset on a rolling window. → What 'running out' looks like: A 'limit reached, resets at *time*'
> message."
> — [Models, usage, and limits in Claude Code](https://support.claude.com/en/articles/14552983-models-usage-and-limits-in-claude-code)

At the wire level, the **Messages API** (what Claude Code speaks to underneath) does have a
structured, machine-readable signal:

> "If you exceed any of the rate limits you will get a 429 error describing which rate limit was
> exceeded, along with a `retry-after` header indicating how long to wait."
> — [Rate limits](https://platform.claude.com/docs/en/api/rate-limits)

Response headers documented: `retry-after`, `anthropic-ratelimit-requests-limit/-remaining/-reset`,
`anthropic-ratelimit-tokens-limit/-remaining/-reset`, plus separate `-input-tokens-*` and
`-output-tokens-*` variants, all RFC 3339 timestamps for reset. Two *different* 429/400 shapes
are explicitly distinguished by the same doc:

> "Once you reach your tier's spend cap ... API requests return HTTP 429 ... The error type is
> `rate_limit_error`, the same as for a rate limit, but the response has no `retry-after` header.
> ... `error.details.error_code` is `enforced_spend_limit_reached`. Use it to tell this response
> apart from a rate limit."

> "When usage reaches a spend limit you set, requests return HTTP 400 with error type
> `invalid_request_error`. The message begins 'You have reached your specified API usage
> limits'..."

> "Limits on the Claude Code workspace are checked separately: Claude Code requests over that
> workspace's limit can instead receive a 429 that carries a `retry-after` header."
> — all three, same page.

That last line is the one concrete, primary-sourced tie between "Claude Code" by name and the
API's structured rate-limit machinery. **But** this is the Console/API-key billing surface, not
confirmed to be the same code path a Pro/Max OAuth subscription hits — **NOT DETERMINED** whether
an OAuth-subscription 5-hour/weekly exhaustion produces this same 429 shape at the transport
level, or a different, undocumented one specific to consumer billing.

---

## 3. Is a rate limit distinguishable from an ordinary failure, from outside the process?

**Yes, a real mechanism exists — but it's generic to all installed agents, not claude-specific,
and its coverage of Claude Code's actual subscription-limit wording is unconfirmed.**

Harbor has a full typed-exception hierarchy for this, defined once in `base.py`, inherited by
every installed agent (`claude_code.py` does **not** override it):

```
base.py:27  NonZeroAgentExitCodeError(RuntimeError)
base.py:33    ApiError
base.py:39      ApiRateLimitError      # "the distinct type name lets retry policy target it"
base.py:50      ApiUsageLimitError     # "an account or project usage limit is exhausted"
base.py:58      ApiInternalServerError
base.py:66      ApiOverloadedError
base.py:74      ApiConnectionClosedError
base.py:82      ApiResponseStalledError
base.py:90      OutputTokenExceededError
base.py:98      ContextWindowExceededError
base.py:106     UnknownApiError
base.py:125     AgentSafetyRefusalError
base.py:138   AgentAuthenticationError
base.py:144   ModelNotFoundError
base.py:152   NetworkConnectionError
```

Classification is a declarative regex table (`ErrorPattern`, `base.py:215-222`),
`base.py:445-521`, compiled once (`base.py:578-581`). The two entries that matter here:

```python
ErrorPattern(r"rate.?limit", ApiRateLimitError)                       # base.py:446
ErrorPattern(r"too many requests", ApiRateLimitError)                 # base.py:447
ErrorPattern(r"specified API usage limits", ApiUsageLimitError)       # base.py:448
ErrorPattern(r"You've hit your usage limit", ApiUsageLimitError)      # base.py:449
ErrorPattern(r"You have an unpaid invoice", ApiUsageLimitError)       # base.py:450
ErrorPattern(r"Quota exceeded.", ApiUsageLimitError)                  # base.py:451
```

**The exec path that feeds it, confirmed specifically for claude_code.py:** the real `claude`
invocation runs through `self.exec_as_agent(...)` (`claude_code.py:1891-1906`, piped
`2>&1 | tee claude-code.txt`) → `exec_as_agent` is a thin wrapper over `self._exec`
(`base.py:889-900`) → `_exec` runs the command as `set -o pipefail; ...` so the pipe through
`tee` doesn't swallow claude's real exit code (`base.py:850`) → on `return_code != 0`, it calls
`self._classify_exec_error(command, result)` (`base.py:865`), which regex-matches the **combined**
stdout+stderr and returns whichever pattern's match ends **furthest into the text** (last-match
wins, not first-in-list — `base.py:797-813`), defaulting to a bare `NonZeroAgentExitCodeError`
if nothing matches (`base.py:814`).

**Where it lands:** this exception propagates out of `run()`, and per the already-established
`harbor-crash-cancel-behaviour.md` finding, Harbor's trial runner catches exactly this class of
exception and records it as `exception_info.exception_type` in the trial's `result.json`
(`ExceptionInfo`, `harbor/models/trial/result.py:20-35`) — the same field that doc already
confirmed holds distinct, real values (`AgentTimeoutError`, `CancelledError`,
`AddTestsDirError`) for other failure modes. So **if** the mechanism fires, dawn's answer is a
single string comparison on JSON already in `result.json` — no CLI-output parsing needed, and
this is a genuinely distinct signal from a timeout (`AgentTimeoutError`) or a cancel
(`CancelledError`).

Retries on this class of exception are opt-in and **off by default**:
`harbor run -r/--max-retries` defaults to 0 (`cli/jobs.py:512-521`); `--retry-include
ApiRateLimitError` (`cli/jobs.py:522-531`) lets a job target retries at this specific type by
name — a real, documented feature, confirming the exception-type distinction is meant to be
actionable, not incidental.

**The gap — three independent reasons this may not fire for a real OAuth-subscription hit:**

1. **Pattern-to-wording mismatch.** None of Anthropic's *documented* Pro/Max/Claude-Code-seat
   strings from section 2 — `"Approaching 5-hour limit."`, `"5-hour limit reached - resets
   [time]."`, `"limit reached, resets at time"` — contain the substrings any `ErrorPattern`
   regex is looking for. `r"specified API usage limits"` matches verbatim the Console/API-key
   **spend-limit** wording quoted in section 2, not a subscription usage-limit message.
   `r"You've hit your usage limit"` does not match any string this research could confirm in
   Anthropic's own docs — **NOT DETERMINED** whether this is real observed CLI output (from some
   version/surface not covered by the docs checked) or defensive coding against an assumed
   phrasing that may not exist. Net: the patterns read as written for **API-key billing errors**,
   not the **consumer subscription quota** dawn actually runs under.
2. **A partial catch-all exists.** claude_code.py's other documented error strings are all
   prefixed `"API Error: ..."` (`base.py:452-475`), and a bare `ErrorPattern(r"API Error",
   UnknownApiError)` sits at `base.py:513`. If Claude Code happens to wrap a subscription-limit
   stop in that same `"API Error: ..."` convention, it would still be classified —
   as `UnknownApiError`, distinguishable from a timeout/crash but **not** specifically labeled
   as scarcity. If it does not carry that prefix (plausible: this is a product-layer message
   from the consumer quota system, not the Messages API error surface Claude Code's other
   `"API Error:"` strings come from), it falls through to a bare `NonZeroAgentExitCodeError` —
   the `exception_type` alone would be indistinguishable from any other crash, though the raw
   limit-reached text would still be sitting, grep-able, inside `exception_message`
   (`base.py:785-789` builds it from truncated stdout/stderr).
3. **Silent retry defeats the mechanism entirely.** This ties to the already-established fact
   that `claude --max-budget-usd 0.001` exited 0 after spending 23× the cap because the check is
   only between turns — evidence Claude Code does not treat every constraint as an immediate
   hard stop. If a subscription limit similarly makes Claude Code pause, backoff, or silently
   degrade rather than exit non-zero, **none** of this classification machinery runs at all
   (`_exec` only classifies `return_code != 0`, `base.py:856`) — the trial would just run long
   or hang, and would surface only as `AgentTimeoutError` if it outlives `timeout_sec`, which is
   indistinguishable, by type, from a genuinely hung task.

**Deliberately not tested:** whether any of the three gaps above actually occurs requires
watching a real subscription hit its limit mid-trial, which the ticket explicitly forbids
exploring. This entire section is source-reading, not experiment, per the ticket's own
constraint — the honest answer to "is it detectable" is **conditionally yes, by construction,
for exceptions the pattern table matches; NOT DETERMINED whether it matches this specific
scarcity's actual wording.**

---

## 4. The one number that matters for planning — extrapolation, clearly labeled

**Measured (n=1, not extrapolated):** `harbor run -p pr-ci -a claude-code`, reward 1.0, 64s wall,
`n_input_tokens: 111092` (91% cache reads, `n_cache_tokens: 101268`), `n_output_tokens: 441`,
`cost_usd: 0.0639436`.

**Everything below this line is extrapolation from that single data point**, combined with the
separately-established finding that token use varies **~30× across identical tasks** — so
scaling a single measurement is close to guessing, not forecasting.

- **Naive linear scaling to a 24-wide MDASH fan-out** (24 independent, isolated containers, each
  doing comparable work): ~24 × $0.064 ≈ **$1.53 notional**, ~24 × 111k ≈ **2.7M input-token-
  equivalent**, wall time roughly unchanged (~64s + orchestration overhead) since the 24 run in
  parallel, not in series.
- **Applying the known ~30× variance** to that same extrapolation collapses any precision: a
  single agent's realistic range spans roughly $0.002–$1.92, so the 24-wide aggregate's honest
  range is closer to **$0.05–$46 notional** — a band wide enough that quoting a point estimate
  would misrepresent what's known from one sample.
- **Dollars are the wrong unit for the actual constraint anyway.** `cost_usd` here is notional —
  either Claude Code's own list-price-equivalent figure or a litellm estimate (section 1); no
  money changes hands on an OAuth subscription. The real ceiling, per sections 2–3, is the
  **shared rolling usage pool** behind the one OAuth token every container in a fan-out
  authenticates with (`docs/research/oauth-cli-in-container.md`: "One token = one seat... Shared
  usage pool... N parallel trials hit rate limits, not scale. Ceiling, not corruption"). Running
  24 containers concurrently means 24 processes drawing on the *same* five-hour/weekly meter
  simultaneously — the aggregate token-equivalent figure above is a reasonable proxy for how
  much of that pool gets consumed, but Anthropic publishes no token size for the pool itself
  (section 2), so **there is no way to compute, only observe, whether a given fan-out width
  exhausts it** — and per section 3, if it does, whether that shows up as a clean, typed
  `ApiUsageLimitError` in each trial's `result.json` or as an opaque hang/timeout is itself
  **NOT DETERMINED**.
- **What would make this a real forecast instead of an extrapolation:** repeated measurements of
  the same task (to bound the 30× variance empirically rather than citing it as a given), and a
  fan-out trial small enough to be safe to actually run and inspect `result.json` for exception
  types — both out of scope here by the ticket's own "do not spend meaningfully, do not run new
  model calls" constraint.

---

## Summary of NOT DETERMINED items

- Whether goose's/OpenCode's self-reported `cost_usd` is meaningful (vs. zero/garbage) under
  OAuth/subscription auth rather than an API key.
- Exact behavior of the ~20 other Harbor adapters beyond the four requested, and of the
  structurally different `computer_1`/`terminus_2` code paths.
- Whether an OAuth-subscription 429 carries the same headers/JSON shape documented for the
  Console/API-key surface, versus an undocumented consumer-billing shape.
- Whether Claude Code's actual subscription-limit stop message matches any Harbor `ErrorPattern`
  (a live test would settle this in one run, but is out of scope here).
- Whether Claude Code exits non-zero at all on a subscription-limit hit, or silently
  backs off/degrades the way it does for `--max-budget-usd`.
- Any real ceiling, in tokens or wall-clock, for how wide a fan-out the shared usage pool
  actually tolerates — Anthropic does not publish the number, so this can only be observed
  empirically, not derived.
