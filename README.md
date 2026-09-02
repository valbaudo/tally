# dawn

**Four facts an agent cannot forge. Everything else is yours.**

Wiped to zero on 2026-09-02. The design is [docs/GLUE-DESIGN.md](docs/GLUE-DESIGN.md);
this repo is at v0-not-started, deliberately.

## What this is

Nineteen teams built vulnerability-research agents on the CyberGym Level 1 board.
The scaffold around the model is worth +4 to +23 points of success rate — more than a
model generation. Every team hand-built the same four things badly, and disagreed about
everything else.

The four:

| Fact | Enforced by | Not by |
|---|---|---|
| what it spent | the proxy is the only route out of the netns | asking |
| whether it succeeded | `class` written only by supervisor-side code | a label check inside the agent |
| shared state generations | refs in a database the container cannot reach | file locks in the workspace |
| its own isolation spec | recorded from the effective container config | trusting the digest field |

Membership rule: a fact belongs inside iff the agent would profit from forging it **and**
the agent's code is the natural place to produce it. Memory fails that rule — authoring
memory is the agent's whole job — so memory is a table, not a primitive.

Refused, permanently: merge policy, projection and ranking, retry policy, memory
semantics, workflow definition, payload schemas, replay/resume. If the nineteen harnesses
disagree about it, it is your code. Control flow is a `for` loop.

## Why the tree is empty

`workflow/`, `scheduler/`, `value/`, `content/`, `workspace/`, `plan/`, `gate/`,
`backend/`, `cmd/` — 13,611 lines — deleted. The scheduler's central `LeafRunner` port
had four test implementations and zero real ones. The plan language typed the payload
(`additionalProperties: false`, every field required, always) and recorded none of cost,
tokens, timing, attempt index or execution position.

`store/`, `proc/` and the flock went too. They build and they are tested, and v0 has no
caller for any of them — and carrying them biases v0 toward using them. The flock is
worse than unused: one run per state directory is the opposite of a sweep.

They come back, with their tests, the day a harness needs them:

```sh
git checkout pre-glue-wipe -- store proc
```

Everything deleted is at `pre-glue-wipe`. Reasoning worth keeping is prose in
[docs/salvage/](docs/salvage/dawn-reasoning.md).

## v0

Two weeks, ~800 lines, target NSFOCUS (95.0%): the LLM proxy, one append-only `events`
table, an atomic budget reserve inside the proxy handler, `glue top`, `glue table`.

No memory, no blobs, no refs, no shim, no `Inject`, no sandbox primitive — the harness
calls `docker run` in ordinary Go.

Keep going if provider-console reconciliation lands within 2%, the submission table needs
zero hand-editing, spend never crosses the cap under 20 workers, and you reach for
`glue top` instead of tailing logs. Stop if building NSFOCUS *on* it costs more than
building it plain — the substrate has to be net-negative for the harness author, or it is
a tax.
