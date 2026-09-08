# Why the agent's `reward.json` does not reach a separate verifier — and when it does

Ticket: tally research #33. Harbor 0.22.0, installed at
`/Users/vabbb/.local/share/uv/tools/harbor/lib/python3.14/site-packages/harbor/` (`$H` below).
Every source line and every command output here was re-run and re-read during adjudication.

**Verdict up front: INCIDENTAL, and already defeated.** Separate-verifier mode is not a security
boundary. It is a housekeeping step that happens to precede a restore, and a single supported
`task.toml` line turns the same code path into a reward forge. Demonstrated, not theorised.

---

## 1. The anomaly

In shared mode an agent that writes `/logs/verifier/reward.json` sets the trial's reward, because
`Verifier.verify()` prefers `reward.json` over the verifier's own `reward.txt`. In separate mode the
same agent write vanishes: the verifier sees nothing, and the host trial dir ends holding only
`reward.txt`. The host path is identical in both modes, so something destroys the file between the
two phases. The question for tally was whether that something is load-bearing.

## 2. The source mechanism

**One host directory, two containers.** The agent mount and the verifier mount compute the same
source expression character for character:

- `$H/trial/trial.py:1633-1635` — `_agent_env_mounts`: `source=self.paths.verifier_dir.resolve().absolute().as_posix()`
- `$H/trial/trial.py:791-795` — `_verifier_env_mounts`: same expression, `target=str(env_paths.verifier_dir)`
- `$H/models/trial/paths.py:247-255` — `verifier_dir` is a plain property, `self.trial_dir / "verifier"`. No key, no step, no phase scoping.
- `$H/trial/trial.py:806-814` — the `key` passed to `_run_separate_verifier` names only the container session (`_separate_verifier_session_id`), never a path. Both modes receive `trial_paths=self.paths` (`trial.py:645`, `trial.py:717`).

**One deletion site.** `$H/trial/trial.py:704`, inside `_run_separate_verifier`, after the container
with the live bind mount is started:

```python
await target_env.empty_dirs([env_paths.verifier_dir], chmod=True)
```

`empty_dirs` (`$H/environments/base.py:626-638`) execs `_empty_dirs_command`
(`$H/environments/base.py:550-587`) as `root` (`_reset_dirs_user`, `base.py:589-593`). Per directory
on POSIX it emits:

```sh
if [ -L /logs/verifier ] || { [ -e /logs/verifier ] && [ ! -d /logs/verifier ]; }; then rm -rf /logs/verifier; fi \
 && mkdir -p /logs/verifier \
 && find /logs/verifier -mindepth 1 -maxdepth 1 -exec rm -rf -- {} + \
 && chmod 777 /logs/verifier
```

The `find ... -exec rm -rf` clause deletes the directory's *contents* through the bind mount, so the
host file dies. The root is never replaced — docstring at `base.py:556`: "Build a shell command that
empties directories without replacing roots" — because replacing it would sever the mount. The
author knew this was a bind mount.

**And one restore, three lines later.** `$H/trial/trial.py:706-712`:

```python
await self._artifact_handler.upload_artifacts(
    target_env, artifacts_dir=artifacts_dir,
    source_artifacts_dir=self.agent_env_paths.artifacts_dir,
    target_artifacts_dir=env_paths.artifacts_dir, artifacts=artifacts,
)
```

`upload_artifacts` (`$H/trial/artifact_handler.py:209-256`) re-materialises every collected artifact
"to its original `source` path". The convention entry `/logs/artifacts` is always injected even with
`artifacts = []` (`$H/models/task/artifacts.py:62-78`, `with_convention_entry`). Directory entries
get their own `empty_dirs` then `upload_dir` (`artifact_handler.py:241-245`); files get
`ensure_dirs` on the parent then `upload_file` (`artifact_handler.py:247-255`). Upload on Docker is
`docker compose cp <hostdir>/. main:<target>` (`$H/environments/docker/docker_unix.py:152-157`).

So the actual order inside one function is: **wipe → restore agent-controlled content → run tests**.

**Reward precedence.** `$H/verifier/verifier.py:227-236` reads the HOST paths:

```python
if self.trial_paths.reward_json_path.exists():
    rewards = self._parse_reward_json()
elif self.trial_paths.reward_text_path.exists():
    rewards = self._parse_reward_text()
else:
    raise RewardFileNotFoundError(...)
```

**Shared mode has no wipe at all.** `_run_shared_verifier` (`$H/trial/trial.py:641-671`) builds the
Verifier against `self.agent_environment` and calls `verify()`. Confirmed by enumerating every
caller: `grep -rn empty_dirs $H --include="*.py"` returns six call sites —
`trial/trial.py:704`, `trial/trial.py:1401` (skills upload), `trial/multi_step.py:337,343,347`,
`trial/artifact_handler.py:241`. The only shared-mode reset is `multi_step.py:342-348`
(`_reset_shared_step_verifier_dirs`), reached from `multi_step.py:205-207` *between steps*. A
single-step shared trial never resets.

**Nothing names this as a defence.** `grep -rni "preempt|tamper|forge" $H --include="*.py"` returns
two hits, neither at line 704: `telemetry.py:342` (unrelated) and `cli/adapter_review.py:615`
("confirm the agent cannot see the ground truth or tamper with scoring" — addressed to task
authors). Harbor's own checklist puts the burden on the task, `$H/cli/adapter_review.py:646`:

> `test.sh` always (re)writes the reward file (`/logs/verifier/reward.txt` or `reward.json`) on
> every code path, overwriting whatever the agent may have written during its run — it never trusts
> a pre-existing reward file

There is no comment at or near `trial.py:704`, and the shipped wheel contains no test suite
(`find` for `tests`/`*test*` beside `$H` returns nothing) — so the shipped package asserts nothing
about the ordering.

## 3. The experiments

Five Harbor trials, `oracle` agent, zero LLM spend. Job dirs under
`/private/tmp/claude-501/-Users-vabbb-Documents-GitHub-tally/81887e3f-f7b1-4d77-aed5-c62deed3707c/scratchpad/provision/jobs/`.
All five trial-level `result.json` files were re-read during adjudication:

| trial | mode | task.toml `artifacts` | verifier writes | result |
|---|---|---|---|---|
| `vb1-t1-probe` | separate | `[]` | `reward.txt=0` | `{"rewards": {"reward": 0.0}}` |
| `vb1-t2-silent` | separate | `[]` | nothing | `verifier_result: null`, `RewardFileNotFoundError` |
| `vb1-t3-fbsep` | separate | `[]` | trusts pre-existing json, else `reward.txt=0` | `{"rewards": {"reward": 0.0}}` |
| `vb1-t4-fbshared` | **shared** | `[]` | same fallback-only script | `{"rewards": {"reward": 1}}` |
| `vb1-t5-artfx` | separate | `["/logs/verifier/reward.json"]` | `reward.txt=0` | **`{"rewards": {"reward": 1}}`** |

In every trial the agent wrote `/logs/verifier/reward.json` = `{"reward": 1}` plus markers all over
the filesystem.

**T1 — what actually crosses the boundary.** The separate verifier's own stdout
(`vb1-t1-probe/vb1-probe__zdeah6q/verifier/test-stdout.txt`):

```
---- find /logs -maxdepth 4 ----
/logs
/logs/verifier
/logs/verifier/test-stdout.txt
/logs/artifacts
/logs/artifacts/escape
/logs/artifacts/logs
/logs/artifacts/logs/verifier/reward.json
/logs/artifacts/marker.txt
/logs/artifacts/reward.json
---- every reward.json visible under /logs ----
[/logs/artifacts/logs/verifier/reward.json] => {"reward": 1}
[/logs/artifacts/reward.json] => {"reward": 1}
---- ls -la /logs/agent ----   ls: cannot access '/logs/agent': No such file or directory
---- ls -la /app ----          ls: cannot access '/app': No such file or directory
---- ls -la /solution ----     ls: cannot access '/solution': No such file or directory
```

Host trial dir after T1:

```
$ ls -la jobs/vb1-t1-probe/vb1-probe__zdeah6q/verifier/
-rw-r--r--  1 vabbb  wheel     2 Sep  4 11:56 reward.txt
-rw-r--r--  1 vabbb  wheel  1832 Sep  4 11:56 test-stdout.txt
```

Reaches a separate verifier: `/logs/artifacts/**`, verbatim, at the same relative path, owned by
uid 501 (it round-tripped through the host). Does **not** reach it: `/logs/verifier/*`,
`/logs/agent/*`, `/app/*`, `/solution`, `/logs/*` outside the convention dir.

**T2 — there is no fallback.** `exception_message`:
`"No reward file found at jobs/vb1-t2-silent/vb1-silent__BEF4A6j/verifier/reward.txt or .../reward.json"`,
`verifier_result: null`. A silent verifier fails the trial; it does not inherit the agent's number.

**T3 vs T4 — the wipe is what does the work.** Identical deliberately-sloppy verifier script
(`if [ -f /logs/verifier/reward.json ]; then trust it; else echo 0 > reward.txt; fi`). Separate mode
logged `no pre-existing reward.json -> writing reward.txt=0` → reward 0.0. Shared mode logged
`PRE-EXISTING reward.json FOUND -> trusting it: {"reward": 1}` → reward 1.0. So T3's 0.0 comes from
`trial.py:704`, not from the verifier being unexploitable.

**T5 — THE BREAK.** One-line diff from T1:

```
$ diff provision/vb1-probe/task.toml provision/vb1-artfx/task.toml
2c2
< artifacts = []
---
> artifacts = ["/logs/verifier/reward.json"]
```

Verifier stdout — it solved nothing and explicitly voted 0, yet saw the agent's file restored:

```
---- ls -la /logs/verifier ----
-rw-r--r-- 1 root root  14 Sep  4 10:00 reward.json      <- agent's, restored after the wipe
---- every reward.json visible under /logs ----
[/logs/verifier/reward.json] => {"reward": 1}
VERDICT: verifier writes reward.txt=0
```

Manifest (`vb1-t5-artfx/.../artifacts/manifest.json`):

```json
{ "source": "/logs/verifier/reward.json",
  "destination": "artifacts/logs/verifier/reward.json",
  "type": "file", "status": "ok", "service": null }
```

Host trial dir ends with **both** files — `reward.json` = `{"reward": 1}` and `reward.txt` = `0` —
and `verifier.py:227` prefers the JSON. `verifier_result: {"rewards": {"reward": 1}}`, mode
`separate`. The agent's number is the verdict.

## 4. Reconciliation

The two agents do **not** contradict each other. Every source prediction was reproduced: same host
path (T1/T4 pair), single deletion site (T3 vs T4), no fallback (T2), reward.txt survives because
it is written after the wipe (T1). The experiment simply had wider scope: the source agent read
`trial.py:706-712` and correctly described it as "before the Verifier is constructed", but did not
draw the line from *artifact restore* to *reward file*. Where they differ in emphasis, trust the
experiment — T5 is a reproduced 1.0 with the manifest and both host files on disk.

Both are right about intent. The reset is deliberate; the protection is a side effect.

## 5. Robust vs incidental — verdict

**INCIDENTAL, and weaker than "a future Harbor could remove it silently".** It is removable *today*,
from Harbor's own supported configuration surface, with no warning and no validation, by a task
author who thinks they are just collecting a debug file.

Attempts to refute "separate mode is safe":

1. **Refuted outright by T5.** A supported `artifacts` entry re-opens the hole completely. A
   directory entry (`["/logs"]`, `["/logs/verifier"]`) would do the same via
   `artifact_handler.py:241-245` — untested, but the same code path. Note that the restore is not an
   accident of ordering someone forgot: `upload_artifacts` is *supposed* to run after the reset, so
   the fresh container gets a clean writable dir and then the collected artifacts. Wipe-then-restore
   is the design.
2. **Even at `artifacts = []`, nothing pins the three properties the defence depends on**: that
   `empty_dirs` precedes `upload_artifacts`; that the tests run after both; that the wipe is
   recursive through a bind mount. No comment, no test in the shipped wheel, no doc. The only
   written-down statement of this threat model (`adapter_review.py:646`) assigns the job to the task
   author, which would be redundant if the framework guaranteed it.
3. **What would have to change in Harbor for the default to become unsafe** — each a refactor no
   reviewer would flag as security-relevant: swap the order of `trial.py:704` and `706`; make the
   reset conditional (skip when the dir "looks clean", or when `artifacts` is empty); change
   `_empty_dirs_command` to `mkdir -p` only, or to `rm -rf` + recreate (which would sever the mount
   and silently decouple the verifier's writes from the host); relax `verifier.py:227` precedence.

**UNVERIFIED / NOT DETERMINED**

- **Symlink traversal from `/logs/artifacts` into `/logs/verifier`.** The agent's
  `/logs/artifacts/escape -> /logs/verifier` is transported verbatim and was inert (T1/T5 stdout,
  and `ls -la` of the host artifacts tree confirms it lands as an absolute, host-dangling symlink).
  Traversal *looks* structurally blocked — nothing can be written beneath a dangling absolute
  symlink on the host, and upload is `docker compose cp` of that host tree — but a **relative**
  symlink (`escape -> ../verifier`, which resolves inside the host trial dir) was never tested.
  Treat as open.
- Whether Harbor's upstream repo has tests asserting the 704/706 ordering. The installed wheel
  ships none; upstream not inspected.
- Non-Docker environments (skypilot, openshift, apple_container). The Docker provider reports
  `capabilities.mounted=True`, so the download branch at `verifier.py:204-216` — which *protects*
  reward filenames from log filters — is never reached here. Other providers untested.
- Multi-step separate-mode trials. Only single-step trials were run.

## 6. Consequence for tally

tally's own vocabulary (`CONTEXT.md`) already commits the gate to "a separate pinned no-network image
after the agent's container is gone". That is necessary and not sufficient. Two lines of policy,
both enforced where tally already writes the task definition — no validation framework, no new
config knobs:

1. **tally emits the `artifacts` list itself, and it never contains a path under `/logs/verifier`.**
   Protocol authors collect debug output from `/logs/artifacts`, which is injected implicitly
   anyway. This is a constant in tally's task.toml writer, not an option.
2. **Every gate image writes `reward.json` unconditionally, as its last act, on every code path** —
   never `reward.txt`, never `if [ ! -f ... ]`. Because the restore happens *before* the tests run
   (`trial.py:704` → `706` → `verify()`), a gate that always writes the highest-precedence file
   overwrites anything the agent got reinstated. Rule 2 closes the hole; rule 1 closes it for the
   case where the gate crashes or times out before writing.

Corollary that costs nothing: a gate that produced no reward file raises `RewardFileNotFoundError`
(T2, verified) — tally maps that to `infra_error`/`unverified`, never to a score. tally already treats
the numeric score as a metric and the State as the verdict; keep it that way.

What tally must *not* do: rely on separate mode alone, or on "the agent can't reach the verifier
directory". It can, through `/logs/artifacts`, whenever the artifact list points anywhere near
`/logs/verifier`.
