import os, textwrap, pathlib
ROOT = pathlib.Path("/Users/vabbb/Documents/GitHub/tally/experiments/harbor-targets/vdh-steps")
STEPS = ROOT / "steps"
ENV   = "tally-vdh-hunt-env@sha256:49f379ebc783f05a6efd07675f3b9ea09f8bbf7c44fef75518d0885ab20541c3"
GATES = {
 "hunt":     "tally-vdh-hunt-gate@sha256:8d9a2c9fa24581c76cc062b2cbfb2d9f0012209ee232b71fea3b69af27b7a9b7",
 "validate": "tally-vdh-validate-gate@sha256:8920f43f31512977b2d58ae698256ca8f4c6bf729f7ac6537a54485f4946a28a",
 "dedupe":   "tally-vdh-dedupe-gate@sha256:dd0472f55516f4d972cc9cbeaa47d13a8fbf7f492492c647cbd3d6e5f53b7772",
 "trace":    "tally-vdh-trace-gate@sha256:321cfdd7662c1d9f1f7f0b22207cd8e604fdc337f6b9a76f2d4c46207e9bc1ad",
 "report":   "tally-vdh-report-gate@sha256:cce0530330d78ff60006ed17775f9d3c5d7ad0e4e64e7552e81415d355fbba65",
}
HUNTERS, ROUNDS = 3, 2

RECON = """The Python package at /app/src is a small data-access layer with request
handlers on top. Read all of it.

Produce a hunting queue: one entry per area worth examining, each naming the
module and the functions in it that build SQL, with a one-line note on how each
gets its query text.

Do not decide yet which are vulnerable - this stage maps the ground.

Write one JSON object:
  {"queue": [{"file": "src/....py", "functions": ["..."], "note": "..."}]}

Write it to /app/outputs/map.json"""

HUNT = """You are hunter {n} of {tot} in a hunt over the Python package at /app/src.

Your input is the hunting queue. Take ENTRY {i} from its "queue" list (zero-based)
and hunt what it names. If the queue is shorter than that, take an area no other
entry covers.

You hunt ONE class: user input reaching SQL text through string building -
f-string, %-formatting, .format(), or concatenation - instead of a bound
parameter.

Hand back ONE function with that flaw and a payload demonstrating it. Your
finding is judged by RUNNING it: the verifier calls the function you name with
the payload you give and accepts only if a row comes back that your payload had
no right to reach, and only if the same function with a harmless payload does
not return it. Try it yourself first.

Note the request handlers in src/api.py take a request mapping, not a bare
string - the bug is one hop further in, and that is where a finding belongs.

A finding is not just a payload. Hand back all four parts:

  {{"file": "src/....py", "function": "...", "payload": "...",
   "why":    "how the input reaches the SQL text",
   "threat": {{"attacker": "who can send this", "boundary": "what it crosses"}},
   "fix":    {{"old": "the exact source text to replace, verbatim",
              "new": "what replaces it"}}}}

The verifier applies your fix to ITS OWN copy and replays the same payload: it
must no longer reach anything, and ordinary input must still work. "old" has to
appear exactly once in the file, so include enough lines to be unambiguous.

Write it to /app/outputs/finding.json"""

VALIDATE = """Your input is ONE hunter's finding. Your job is to DISPROVE it.

Read /app/src and attack the claim: is the input really attacker-controlled, does
it really reach SQL text unsanitized, does the payload really change the query's
meaning rather than merely returning rows?

You may not report a finding of your own. Your verdict is about THIS finding and
this one only, and the verifier refuses a verdict naming anything else.

Write one JSON object, echoing the file and function you were given:
  {"file": "...", "function": "...", "verdict": "confirmed" | "refuted", "why": "..."}

Write it to /app/outputs/verdict.json"""

GAPFILL = """Your inputs are under /app/inputs, one directory per stage that produced them:
the hunting queue a round worked from, and the adversaries' verdicts on what
that round found. Read /app/src as well.

A finding an adversary REFUTED leaves its area still uncovered - treat it as a
gap, not as ground already walked.

Find the GAP: functions or modules the round touched but did not cover, and
anything no hunter looked at. Emit the queue for the next round, aimed only at
what is still unexamined.

Write one JSON object:
  {"queue": [{"file": "src/....py", "functions": ["..."], "note": "why this is still open"}]}

Write it to /app/outputs/map.json"""

DEDUPE = """Your inputs are under /app/inputs, one directory per hunter that produced a
finding. Some describe the SAME bug.

Collapse them, and DECLARE the collapsing: each finding you keep lists the
inputs it stands for, including itself. Two reports of one bug become one
finding absorbing both - even where they name different functions, if you judge
them the same bug.

The verifier checks your grouping is a partition: every input absorbed by
exactly one keeper, nothing absorbed that was never handed in. It does not
second-guess WHICH things you judged equivalent - but it will not let work be
silently dropped.

Write one JSON object:
  {"findings": [{"file": "src/....py", "function": "...", "why": "...",
                 "absorbed": [{"file": "...", "function": "..."}]}]}

Write it to /app/outputs/deduped.json"""

TRACE = """Your input is the deduped findings. Read /app/src, including the request
handlers in src/api.py, which are the outside edge.

For EACH finding, give the call path by which attacker-controlled input reaches
it, starting at a handler and ending at the vulnerable function itself. Every
consecutive pair must be a call this source actually makes - the verifier parses
the code and checks each hop.

Write one JSON object:
  {"traces": [{"function": "...", "path": ["handler", "...", "the function"], "why": "..."}]}

Write it to /app/outputs/trace.json"""

FEEDBACK = """Your inputs are the confirmed findings and their reachability paths.

A bug found in one place is a hunting task everywhere the same shape could
occur. From what was confirmed, emit a queue aimed at the places NOT yet
confirmed - same class, different call site, or the same handler reached a
different way.

Write one JSON object:
  {"queue": [{"file": "src/....py", "functions": ["..."], "note": "what pattern sent you here"}]}

Write it to /app/outputs/map.json"""

REPORT = """Your inputs are the confirmed findings, their reachability paths, and the
follow-up queue. Read /app/src yourself.

Produce the final report: the functions really vulnerable to the seeded class -
user input reaching SQL text through string building instead of a bound
parameter.

PRECISION IS SCORED, not just recall. Every vulnerable function here has a safe
sibling doing the analogous read with a bound parameter, and the request
handlers build no SQL at all. Listing everything scores worse than listing what
you can defend.

Write one JSON object:
  {"findings": [{"file": "src/....py", "function": "...", "why": "..."}]}

Write it to /app/outputs/report.json"""

# (name, gate_key_or_None, instruction, artifact)
steps = [("recon", None, RECON, "map.json")]
for r in range(ROUNDS):
    for i in range(HUNTERS):
        steps.append((f"hunt-r{r}-{i}", "hunt", HUNT.format(n=i+1, tot=HUNTERS, i=i), "finding.json"))
    for i in range(HUNTERS):   # UNROLLED AT WORST CASE: tally uses len(live)
        steps.append((f"validate-r{r}-{i}", "validate", VALIDATE, "verdict.json"))
    if r + 1 < ROUNDS:
        steps.append((f"gapfill-r{r}", None, GAPFILL, "map.json"))
steps.append(("dedupe", "dedupe", DEDUPE, "deduped.json"))
steps.append(("trace",  "trace",  TRACE,  "trace.json"))
steps.append(("feedback", None,   FEEDBACK, "map.json"))
for i in range(HUNTERS):
    steps.append((f"hunt-r{ROUNDS}-{i}", "hunt", HUNT.format(n=i+1, tot=HUNTERS, i=i), "finding.json"))
for i in range(HUNTERS):
    steps.append((f"validate-r{ROUNDS}-{i}", "validate", VALIDATE, "verdict.json"))
steps.append(("report", "report", REPORT, "report.json"))

STEPS.mkdir(parents=True, exist_ok=True)
for name, gate, instr, art in steps:
    d = STEPS / name
    d.mkdir(parents=True, exist_ok=True)
    (d / "instruction.md").write_text(instr + "\n")

toml = [
 'schema_version = "1.4"',
 'artifacts = ["/app/outputs"]',
 '# tally returns the LAST stage\'s state as the run\'s state (protocols/vdh/main.go',
 '# returns report.State), so "final" is the closest available match. It is not the',
 '# same thing: tally also reads every intermediate gate\'s metrics for control flow,',
 '# which no aggregation strategy exposes.',
 'multi_step_reward_strategy = "final"',
 '',
 '[task]',
 'name = "tally/vdh-steps"',
 'version = "1.0.0"',
 'description = "vdh translated to Harbor [[steps]] - see README.md for what did not survive"',
 'keywords = []',
 '[[task.authors]]',
 'name = "Valerio Baudo"',
 '',
 '[metadata]',
 '',
 '[environment]',
 f'docker_image = "{ENV}"',
 'network_mode = "no-network"',
 'build_timeout_sec = 600.0',
 'os = "linux"',
 'mcp_servers = []',
 '',
 '[environment.env]',
 '',
 '[agent]',
 'network_mode = "allowlist"',
 'allowed_hosts = ["api.anthropic.com", "platform.claude.com"]',
 'timeout_sec = 1200.0',
 '',
 '[verifier]',
 'timeout_sec = 300.0',
 'network_mode = "no-network"',
 'collect = []',
 '',
 '[solution.env]',
 '',
]
for name, gate, instr, art in steps:
    toml += ['', '[[steps]]', f'name = "{name}"', f'artifacts = ["/app/outputs/{art}"]']
    if gate:
        toml += [
          '# min_reward 1.0 mirrors tally\'s classify(): reward > 0 is Passed. But tally',
          '# REJECTS the stage and keeps going per its own control flow, while Harbor can',
          '# only abort every remaining step.',
          'min_reward = 1.0',
          '[steps.verifier]',
          'environment_mode = "separate"',
          'timeout_sec = 300.0',
          '[steps.verifier.environment]',
          f'docker_image = "{GATES[gate]}"',
          'network_mode = "no-network"',
        ]
    else:
        toml += [
          '# tally marks this NoGate and clamps it to `unverified`, a distinct state.',
          '# Harbor has no per-step "no verifier" mode: verifier.disable lives on the',
          '# TRIAL, so an ungated step either inherits the task verifier or has none of',
          '# its intent recorded. The reason tally carries in NoGate("...") has nowhere',
          '# to go at all.',
        ]
ROOT.joinpath("task.toml").write_text("\n".join(toml) + "\n")
print(f"wrote {len(steps)} steps")
for name, gate, _, _ in steps:
    print(f"  {name:18} gate={gate or '-- NoGate, inexpressible --'}")
