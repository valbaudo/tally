# Harbor crash/cancel behaviour — dawn ticket #15

What survives when the process that started a Harbor trial dies mid-run, and what a later
process can reconstruct by inspection alone (no replay). Four cases run end-to-end against
real Docker/OrbStack, plus the relevant Harbor 0.22.0 source read from disk.

- **Harbor version:** 0.22.0 (`harbor --version`)
- **Source root:** `/Users/vabbb/.local/share/uv/tools/harbor/lib/python3.14/site-packages/harbor/`
- **Docker:** OrbStack, server 29.4.0, `docker compose` v5.1.2
- **Method:** every command below was actually run; every output is pasted verbatim (trimmed
  only for length). Nothing here is inferred from documentation.

## Setup

Two throwaway tasks, both copies of the provisioned `smoke` task
(`/private/tmp/.../scratchpad/provision/smoke`, oracle solution writes `dawn` to
`/app/answer.txt`, verifier greps it):

- **`crash-long`** — `solution/solve.sh` changed to `sleep 120` before writing the answer;
  `[agent] timeout_sec` and `[verifier] timeout_sec` raised to 900s so the sleep is never the
  thing that ends the trial (cases 1–3).
- **`crash-timeout`** — `solve.sh` does `sleep 60`; `[agent] timeout_sec = 10.0` (case 4).

All jobs run from `/private/tmp/claude-501/-Users-vabbb-Documents-GitHub-dawn/81887e3f-.../scratchpad/provision`
with `harbor run -p <task> -a oracle -o jobs --job-name crash-<case> -y`, `oracle` agent (no LLM
spend). Every job name is prefixed `crash-`; every container Harbor creates inherits that prefix
via the compose project name.

### A methodological detour that turned into a finding

The ticket asks for `kill -INT` to simulate Ctrl-C. In this Bash-tool environment, a background
job's SIGINT (and, it turns out, SIGTERM sent to a `trap`-holding `bash -c` wrapper) is silently
swallowed — confirmed independently of Harbor with a bare `sleep`/`trap` probe:

```
$ bash -c 'trap "touch /tmp/int_marker; exit 1" INT; sleep 30' &   # backgrounded, then in a
$ kill -INT <pid>                                                  # separate shell invocation
$ ls /tmp/int_marker
ls: /tmp/int_marker: No such file or directory   # trap never fired, process still alive
```

This is standard POSIX job-control behaviour: a shell running an asynchronous (`&`) command
without job control sets SIGINT/SIGQUIT to `SIG_IGN` for that command, and CPython's interpreter
startup (`PyOS_InitInterrupts`) leaves an inherited `SIG_IGN` disposition alone rather than
overriding it. A plain `kill <pid>` (default SIGTERM, no trap) on a bare backgrounded process
*did* work immediately, confirming the delivery path itself is fine — it's specifically
SIGINT-on-a-background-job that's a no-op here, in this harness and in a real user's shell alike.

Two `kill -INT` runs against a live `harbor run` process (`crash-sigint`, `crash-sigint2`,
including one where SIGINT was explicitly reset to `SIG_DFL` before `exec`) both **ran to full
completion, reward 1.0, unaffected** — proof harbor also does not install its own SIGINT
handler (confirmed in source below), so it inherits exactly this default. Case 2 below therefore
uses **SIGTERM**, which is the signal harbor *does* explicitly wire up
(`cli/jobs.py:1974`) and which is deliverable in any shell, interactive or not. This is the
realistic proxy for "a supervisor/user asks the trial to stop" — a real interactive Ctrl-C from
a foreground terminal would also reach Python's default SIGINT handler as KeyboardInterrupt
(foreground jobs don't get the SIG_IGN treatment), so SIGTERM's behaviour is what matters and
generalizes.

---

## Case 1 — hard crash of the parent (`kill -9`)

```
$ cd .../provision
$ nohup harbor run -p crash-long -a oracle -o jobs --job-name crash-hardkill -y > /tmp/crash-hardkill.harbor.out 2>&1 &
PID=51982
$ docker ps --filter name=crash --format 'table {{.Names}}\t{{.Status}}'
crash-long__yfn8vry__env-main-1   Up 16 seconds
$ ps -o pid,ppid,command -p 51982; pgrep -P 51982
  PID  PPID COMMAND
51982     1 .../harbor run -p crash-long -a oracle -o jobs --job-name crash-hardkill -y
52097
$ kill -9 51982
$ ps -p 51982
  PID TTY TIME CMD          # empty — confirmed dead
```

**The child survives the parent's SIGKILL.** `52097`/`52099` (`docker compose ... exec ... bash -c
(/solution/solve.sh) > /logs/agent/oracle.txt 2>&1`) were reparented to launchd (`ppid 1`) and
kept running untouched:

```
$ ps -p 52097 -o pid,ppid,command
  PID  PPID COMMAND
52097     1 docker compose --project-name crash-long__yfn8vry__env ... exec ...
```

**On disk immediately after the kill:** `config.json`, `lock.json`, `trial.log` (one line:
`Skipping image OS validation...`), `agent/oracle.txt` (one partial line: `solve.sh: sleeping
120s...`). **No `result.json` at the trial level, no `exception.txt`, empty `verifier/`.** The
job-level `result.json` is frozen mid-run and never gets a `finished_at`:

```json
{
    "started_at": "2026-09-04T12:57:31.217386",
    "updated_at": "2026-09-04T10:57:31.295748Z",
    "finished_at": null,
    "stats": { "n_running_trials": 1, "n_completed_trials": 0, ... }
}
```

**105 seconds later** (past the in-container `sleep 120`), the orphaned `docker compose exec`
process finished on its own and exited — but `agent/oracle.txt` never grew, because `solve.sh`'s
only stdout write was its first line; the actual effect (`echo dawn > /app/answer.txt`) happened
*inside* the container, invisible to any host-side log:

```
$ docker exec <container> cat /app/answer.txt
dawn
```

**13+ minutes later, still running, `RestartPolicy=no`:**

```
$ docker ps --filter name=crash-long__yfn8vry --format 'table {{.Names}}\t{{.Status}}'
crash-long__yfn8vry__env-main-1   Up 13 minutes
$ docker network ls | grep crash
36d1f1329b7f   crash-long__yfn8vry__env_default   bridge    local
```

The container is orphaned indefinitely — nothing reaps it — and so is its compose network. No
named volume leaked (this task declares none).

**Reconstructable by inspection?** From the job directory alone: **no.** There is no
`result.json`, no `exception.txt`, nothing distinguishing "still legitimately running" from
"orphaned after a crash" except a stale job-level `result.json` whose `updated_at` stops
advancing. The only way we determined the agent had actually succeeded was by `docker exec`-ing
into the still-alive orphan and reading `/app/answer.txt` directly — a live-inspection trick
that (a) is not durable (the container can be reaped at any moment by anyone and takes the
evidence with it), and (b) only worked because the task happened to leave its answer in a place
we knew to look; the task's `task.toml` here declares `artifacts = []` and `[verifier] collect =
[]`, so nothing was configured to be durably collected. Confirmed: `find jobs/crash-hardkill
-name manifest.json` returns nothing — the artifact-manifest step (`trial.py` `_finalize`, only
reached via the `finally:` block) never ran.

**Why:** `trial.py:401-431` (`Trial.run`) wraps the whole trial in `try/except
asyncio.CancelledError/except Exception/finally: await self._finalize()`. A `SIGKILL` to the
Python process gives none of that code a chance to run — there is no signal, no exception, no
`finally`, the process image simply vanishes. `_finalize` (`trial.py:455-461`) is what writes
`result.json` (`self.paths.result_path.write_text(...)`) and calls
`_stop_agent_environment` (`trial.py:1606`, itself `asyncio.shield`-protected against
cancellation) which calls `DockerEnvironment.stop()` (`docker.py:941`), which is what runs
`docker compose down`. None of it executes.

---

## Case 2 — graceful cancel (SIGTERM, harbor's own cancel path)

```
$ nohup harbor run -p crash-long -a oracle -o jobs --job-name crash-sigterm -y > out 2>&1 &
PID=72173
$ docker ps | grep crash-long          # crash-long__rzeez2h__env-main-1  Up 10 seconds
$ kill -TERM 72173                      # 13:08:55
... 14s later ...
$ ps -p 72173                           # gone, 13:09:09
$ docker ps -a --filter name=crash-long__rzeez2h
NAMES     STATUS                        # empty — container fully cleaned up
```

`harbor.out`:
```
  1/1 Mean: 0.000 ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━ 0:00:29 0:00:00
```
(the 29s displayed is the visible progress-bar tick, not total runtime; the process took ~14s
from signal to exit.)

Job-level `result.json`:
```json
"stats": { "n_completed_trials": 1, "n_errored_trials": 1, "n_cancelled_trials": 1,
  "evals": {"oracle__adhoc": {"exception_stats": {"CancelledError": ["crash-long__rZeEz2H"]}}}}
```

Trial dir (`jobs/crash-sigterm/crash-long__rZeEz2H/`): `config.json`, `lock.json`, `trial.log`,
**`exception.txt`** (5KB traceback), **`result.json`**, `agent/oracle.txt`, `verifier/` (empty —
verifier never started), `artifacts/manifest.json`.

`trial.log`:
```
Skipping image OS validation for hb__9a62b5c0d6e2de28d19a8ca1d32b759e: docker inspect returned 1
Trial crash-long__rZeEz2H cancelled
Collecting main service artifacts
```

`exception.txt` (head and the load-bearing frame):
```
  File ".../cpython-3.14.6.../selectors.py", line 548, in select
    kev_list = self._selector.control(None, max_ev, timeout)
  File ".../harbor/cli/jobs.py", line 359, in _handle_sigterm
    raise KeyboardInterrupt
KeyboardInterrupt
...
  File ".../harbor/environments/docker/docker.py", line 675, in _collect_buffered_output
    stdout_bytes, stderr_bytes = await asyncio.wait_for(
        process.communicate(...), timeout=timeout_sec)
...
asyncio.exceptions.CancelledError
```

`result.json` (trial level) — `exception_info.exception_type: "CancelledError"`,
`verifier_result: null`, `started_at`/`finished_at` both present, `agent_execution.finished_at`
matches the moment of cancellation.

**Reconstructable by inspection? Yes, completely.** `result.json` + `exception.txt` unambiguously
say: agent phase in progress, cancelled, verifier never ran, no reward. Container and network
are gone (`environment.delete=true` in this run's config → `docker compose down --rmi local
--volumes --remove-orphans`, `docker.py:958-963`).

**Why it's clean:** `cli/jobs.py:1974` registers `signal.signal(signal.SIGTERM,
_handle_sigterm)`; the handler (`cli/jobs.py:358-359`) just `raise KeyboardInterrupt`. That
interrupts whatever coroutine is running (here, `_run_docker_compose_command`'s
`process.communicate()`, per the traceback), asyncio's default machinery turns the interrupted
await into `CancelledError`, `Trial.run`'s `except asyncio.CancelledError` branch
(`trial.py:409-416`) fires, and — critically — `finally: await self._finalize()`
(`trial.py:422-426`) still runs, and `_stop_agent_environment` (`trial.py:1606-1621`) wraps its
call to `agent_environment.stop()` in `asyncio.shield`, so even if *another* cancellation lands
mid-cleanup, the docker teardown still completes. That shield is the reason case 2 leaves nothing
behind while case 1 leaves everything behind: the difference is entirely "did any Python code run
at all," not "how graceful is the cleanup code."

---

## Case 3 — container killed out from under a running trial

```
$ nohup harbor run -p crash-long -a oracle -o jobs --job-name crash-rmunder -y > out 2>&1 &
PID=74785
$ docker ps | grep crash-long     # crash-long__g6r2idt__env-main-1  Up 12 seconds
$ CID=$(docker ps -q --filter "name=^crash-long__g6r2idt__env-main-1$")
$ docker rm -f "$CID"             # 13:09:53, container a4ce8ecf20c1
$ ps -p 74785                     # gone within 5s, 13:09:58
```

`harbor.out`:
```
┃ Trials ┃ Exceptions ┃  Mean ┃
│      0 │          1 │ 0.000 │
┃ Exception        ┃ Count ┃
│ AddTestsDirError │     1 │
Total runtime: 19s
```

The harbor process **did not crash** — it caught the failure, wrote full artifacts, and exited
cleanly in ~5s after the `rm -f`.

`agent/exit-code.txt`: **`137`** (128+9 — the exec'd `sleep 120` was itself SIGKILLed the instant
the container was force-removed). This is the single most useful on-disk signal in this whole
report: a plain integer file that says, unambiguously, "something killed this process with
signal 9," distinguishable from a normal 0 or a task-level non-zero failure.

`exception.txt` — three chained exceptions, all typed, none a bare crash:
```
RuntimeError: Docker compose command failed ... cp ... main:/tests. Return code: 1.
  Stdout: no container found for service "main"
RuntimeError: Docker compose cp failed, and tar upload fallback also failed. ...
  Stdout: service "main" is not running
harbor.verifier.verifier.AddTestsDirError: Failed to add tests directory to environment.
```
Raised at `verifier/verifier.py:150-155`, via `docker_unix.py:154` (`upload_dir`) →
`docker.py:656` (`_run_docker_compose_command`, which turns a non-zero compose exit code into a
`RuntimeError`) → the tar-fallback path (`docker_unix.py:134-136`) also fails for the same
underlying reason (no container) → `AddTestsDirError`.

`result.json`: `agent_execution` window is `11:09:37.535`–`11:09:53.328` (≈16s, ending exactly
when we `rm -f`'d, confirming the exec call returned — with a failure — the instant the container
disappeared, rather than hanging). `exception_info.exception_type: "AddTestsDirError"`.
`verifier_result: null` (verifier step itself never got past uploading `/tests`).

Job-level `result.json`: `n_errored_trials: 1`, `exception_stats: {"AddTestsDirError": [...]}`.

Post-mortem `docker ps -a` for this job's project: empty — nothing to clean up, since we already
removed the only container ourselves and harbor's own teardown (`environment.stop(delete=True)`)
ran cleanly afterward for whatever compose state remained (network etc.).

**Reconstructable by inspection? Yes, completely**, and unusually precisely: the exit code
(137) plus the typed `AddTestsDirError` together tell a later process exactly what class of
infrastructure fault occurred — not "the agent failed the task," not "verifier rejected the
work," but "the substrate disappeared out from under a still-running attempt."

---

## Case 4 — agent timeout (`timeout_sec=10`, `sleep 60`)

```
$ harbor run -p crash-timeout -a oracle -o jobs --job-name crash-timeout-job -y
... 24s total, synchronous run, no signals sent ...
┃ Reward ┃ Count ┃      │ 0.0    │     1 │
┃ Exception         ┃ Count ┃   │ AgentTimeoutError │     1 │
```

Unlike cases 1–3, **the verifier still ran.** `verifier/reward.txt` = `0`,
`verifier/test-stdout.txt` = `FAIL: got ''` (the answer file was never written — `solve.sh`
never got past its 60s sleep before being timed out at 10s).

`result.json`:
```json
"exception_info": {"exception_type": "AgentTimeoutError",
  "exception_message": "Agent execution timed out after 10.0 seconds"},
"verifier_result": {"rewards": {"reward": 0.0}},
"agent_execution": {"started_at": "...31.081452Z", "finished_at": "...41.091790Z"}
```
`agent_execution` duration is 10.01s — the configured budget, exactly. No `agent/exit-code.txt`
this time (the oracle agent's normal completion path, which writes that file, never runs because
`asyncio.wait_for` abandons the await instead of letting the subprocess return).

`exception.txt` traceback root: `asyncio.timeouts.timeout.__aexit__` raises `TimeoutError`,
caught and re-raised as `harbor.trial.errors.AgentTimeoutError` at `trial.py:554`
(`_run_agent_phase`, wrapped `asyncio.wait_for(..., timeout=timeout_sec)` at `trial.py:545`).

`docker ps -a` after the job finished: nothing left for `crash-timeout` — normal teardown ran (the
same `_finalize`/`stop(delete=True)` path as any completed trial; `AgentTimeoutError` is not a
signal-based death, so nothing prevents the `finally:` block from running).

**Note on what the timeout actually stops:** `asyncio.wait_for`'s cancellation abandons the
*host-side await* on the exec call; it does not, by itself, prove the in-container `sleep 60` was
killed at that instant — the container's own teardown (`docker compose down`, seconds later as
part of normal `_finalize`) is what actually terminates any lingering process. We did not manage
to catch the container mid-timeout to inspect this directly (the whole run window is ~10–15s); this
detail is a source-level inference (`trial.py:541-554`), not something separately observed on the
wire — flagging so it isn't over-claimed as directly verified.

**Reconstructable by inspection? Yes, completely, and distinguishable from case 1's crash and
case 3's substrate-removal along two independent axes**: `exception_info.exception_type ==
"AgentTimeoutError"` names the cause precisely, and — uniquely among all four cases —
`verifier_result` is non-null, because the timeout only aborts the agent step; the verifier still
gets a chance to grade whatever state exists.

---

## Cross-case summary

| Case | Harbor process | Container | `result.json` (trial) | `exception.txt` | Verifier ran? | Reconstructable by inspection |
|---|---|---|---|---|---|---|
| 1. `kill -9` parent | dies instantly, no cleanup code runs | **orphaned, runs forever** (+ orphaned network) | **absent** | absent | no | **No** — job-level `result.json` frozen mid-run is the only signal; agent's actual completion was only visible by live `docker exec` into the still-running orphan, which is not durable and wasn't collected as an artifact |
| 2. SIGTERM (graceful) | exits in ~14s via `KeyboardInterrupt`→`CancelledError` | removed (`compose down --rmi local --volumes --remove-orphans`) | present, `exception_type=CancelledError` | present (5KB) | no | **Yes** |
| 3. `docker rm -f` container | exits in ~5s, catches the fault as a typed exception | already gone (we removed it) | present, `exception_type=AddTestsDirError` | present, chained | no (fails uploading `/tests`) | **Yes**, and precisely — `agent/exit-code.txt=137` pins the cause |
| 4. agent timeout | exits normally (~24s total) | removed via normal teardown | present, `exception_type=AgentTimeoutError` | present | **yes**, reward=0.0 | **Yes**, and uniquely carries a real (if pessimistic) verifier verdict |

The dividing line is not "signal vs. exception vs. timeout" — it's **whether any Python code in
the harbor process ran at all**. Every failure mode where the harbor process itself stays alive
long enough to hit a `finally:` — however abruptly the failure arrived — leaves a fully
reconstructable trial dir. The only unrecoverable-by-inspection case is the one where the process
is killed before it can run any of its own code.

---

## What an attempt journal must record before dispatch

Recorded the instant dawn spawns the `harbor run` subprocess for an attempt — not after, since
case 1 proves nothing written by harbor itself can be trusted to exist later:

1. **`harbor_pid` + a PID-reuse guard (process start time / a `/proc`-equivalent fingerprint).**
   Rescues case 1: the job-level `result.json` never gets a `finished_at` and its `updated_at`
   simply stops advancing — there is no event, no exit code, nothing harbor writes that tells a
   later process the runner died. A liveness check (`kill -0` + start-time match, to rule out a
   *different* process having since reused the PID) is the only way to detect "this attempt's
   supervisor is gone" at all.

2. **`compose_project_name`**, computed with the exact same sanitizer harbor uses
   (`_sanitize_docker_compose_project_name`, `docker.py:86-99` — lowercase, non-`[a-z0-9_-]`→`-`,
   leading non-alnum gets a `0` prefix) applied to the trial's `session_id`. The main container is
   then deterministically `{project}-main-1` (observed: `crash-long__yfn8vry__env-main-1`) and its
   network `{project}_default`. Rescues cases 1 and 3: this is what lets a reaper find, inspect, and
   safely remove *exactly* the container/network a given attempt owns — the same precision this
   ticket's own safety rules demand ("identify precisely," "never `docker rm -f $(docker ps -aq)`").
   Without recording this up front, a reaper is reduced to fuzzy name-prefix guessing.

3. **`job_dir` / expected trial directory path.** Rescues all four cases: it's where to go look,
   and its mere *emptiness of `result.json`* (case 1) vs. *presence* (2–4) is itself the primary
   signal distinguishing "crashed" from "finished, however badly."

4. **`dispatched_at` and the resolved `agent.timeout_sec` + `verifier.timeout_sec` budget** (from
   the task's own `task.toml`, before any multiplier surprises at runtime). Rescues case 4 and,
   jointly with #1, case 1: gives dawn an independent deadline after which a still-"alive"-looking
   PID should stop being trusted, and lets dawn tell "still legitimately running" apart from
   "should have finished by now, go check."

5. **`task_checksum`/digest** — the same value harbor itself computes and stores
   (`result.json.task_checksum`, and `lock.json`'s per-trial `task.digest`,
   `models/job/lock.py:63-79` `_validate_digest`/`_prefixed_digest`). Rescues a subtler failure
   than any of the four cases: if the task definition on disk changes between dispatch and a later
   reap-time inspection (a real risk for anything long-lived enough to survive a crash, per case
   1), this is what lets the inspector confirm a surviving container is still evidence for the
   task version it thinks it's checking, not a stale mismatch.

6. **Which files the task's `[verifier] collect` / top-level `artifacts` declare** (task.toml).
   This is the sharpest finding in this report: **`crash-long`'s `task.toml` declares
   `artifacts = []` and `collect = []`, so in every case where the container was actually torn
   down (2, 3, 4) the agent's own output file (`/app/answer.txt`) was never durably captured** —
   confirmed by inspecting every case's `artifacts/manifest.json`, each showing only the empty
   default `/logs/artifacts` convention directory, nothing task-specific. Case 1's manifest.json
   doesn't even exist (crash preceded artifact collection). The only reason case 1's outcome was
   knowable at all was a live, undocumented `docker exec` into a container that happens to still
   be running — a lucky accident of timing, not a designed recovery path. If dawn wants inspection
   (rather than replay) to actually work, protocol authors must declare `collect=[...]` for
   whatever evidence a later regrade needs, **and** the journal should record that declaration so
   dawn knows in advance whether inspection-based recovery is even possible for a given attempt,
   versus needing a full agent re-run.

   Aside, found while reading source, not exercised here: harbor ships a first-class
   **regrade** path (`harbor/trial/regrade.py:1-13`) that re-runs *only* the verifier against a
   recorded trial's `agent/`+`artifacts/`, seeded from the artifact manifest, without touching the
   agent — exactly the "reconstruct rather than replay" shape dawn wants, gated entirely on
   whether the source trial's manifest actually contains what the new verifier declares it needs
   (`regrade.py:8-12`). This is the durable, designed version of what case 1's live-container
   `docker exec` did by luck.

---

## Does Harbor leave orphaned containers/volumes after a hard crash?

**Yes, confirmed.** 13+ minutes after `kill -9`, `crash-long__yfn8vry__env-main-1` was still
`Up`, `RestartPolicy=no` (`docker inspect`), and its compose network
(`crash-long__yfn8vry__env_default`) was also still present. Nothing in Docker/OrbStack, and
nothing in Harbor (no external supervisor, no restart policy, no reaper of its own), cleans this
up. Every teardown path we found — normal completion, SIGTERM-cancel, timeout, even the
already-removed-container case — routes through `Trial._finalize` →
`_stop_agent_environment` → `DockerEnvironment.stop()` (`trial.py:455`, `1606`, `docker.py:941`),
which only runs from inside the harbor process's own `finally:` block. A `SIGKILL`\-equivalent
death (OOM kill, `kill -9`, a supervisor that force-stops without a grace period) bypasses all of
it. **dawn needs its own reaper** — something external to any single `harbor run` process,
sweeping by the compose-project-name convention (#2 above) against dawn's own attempt journal
(a container/network whose project name doesn't correspond to a journal entry dawn still
considers in-flight is a leak, full stop) — since nothing in Harbor or Docker provides this on its
own.

---

## Mapping to dawn's six states

(`passed` / `rejected` / `unverified` / `exhausted` / `infra_error` / `cancelled`, per
`/Users/vabbb/Documents/GitHub/dawn/CONTEXT.md`.)

- **Case 1 (hard kill -9):** `infra_error`. Not `unverified` — that would imply dawn trusts the
  agent-side outcome and is only missing a verifier verdict; here dawn's own bookkeeping (a dead
  PID, an empty/absent trial `result.json`) can't even establish that much without the lucky,
  non-durable live-container inspection. The runner itself failed; redispatch, don't grade.
- **Case 2 (SIGTERM/graceful cancel):** `cancelled`. Unambiguous — harbor's own vocabulary agrees
  (`exception_type: "CancelledError"`, `trial.log`: "Trial ... cancelled").
- **Case 3 (container removed underneath):** `infra_error`. The agent never got a fair run (its
  exec was SIGKILLed by the removal, exit 137) and the verifier never got to grade anything real
  — this is a substrate fault, not a task-quality signal.
- **Case 4 (agent timeout):** `exhausted`. The agent ran out of its allotted time budget; this is
  a budget-exhaustion outcome, not an infrastructure fault (harbor's own teardown was completely
  clean) and not a genuine content judgment. Note the nuance: harbor *does* hand back a real
  `verifier_result` (reward 0.0) in this case, since the verifier still ran against whatever
  state existed — a policy could reasonably choose to read that as `rejected` instead if it wants
  to trust the verifier's word over the budget framing. We map it to `exhausted` because the
  ticket's own framing groups "wall clock, budget cut" together, and because `agent_execution`'s
  duration (10.01s, exactly the configured budget) is the dominant, unambiguous signal here.

---

## Source citations (file:line, all read directly from the installed 0.22.0 package)

- `trial/trial.py:401-431` — `Trial.run()`: the `try/except CancelledError/except Exception/finally: self._finalize()` structure that everything above hinges on.
- `trial/trial.py:455-461` — `_finalize()`: writes `result.json`, calls `_stop_agent_environment()`.
- `trial/trial.py:1606-1621` — `_stop_agent_environment()`: `asyncio.shield`-wrapped call into `DockerEnvironment.stop()`.
- `trial/trial.py:487-554` — `_run_agent_phase`: `asyncio.wait_for(..., timeout=timeout_sec)` around the agent's exec, raising `AgentTimeoutError` (line 554) on expiry.
- `trial/trial.py:641-680` — `_run_shared_verifier`: same `wait_for` pattern for the verifier's own timeout.
- `verifier/verifier.py:150-155` — where a torn-away container turns into `AddTestsDirError`.
- `environments/docker/docker.py:86-99` — `_sanitize_docker_compose_project_name`, the exact naming rule a reaper must reproduce.
- `environments/docker/docker.py:941-970` — `DockerEnvironment.stop()`: `keep_containers` → `compose stop`; `delete` → `compose down --rmi local --volumes --remove-orphans`; else plain `compose down`.
- `environments/docker/docker.py:656-675` — `_run_docker_compose_command`/`_collect_buffered_output`: how a non-zero compose exit becomes a `RuntimeError`, and where `CancelledError` surfaces from a SIGTERM.
- `cli/jobs.py:358-359` — `_handle_sigterm`: `raise KeyboardInterrupt`.
- `cli/jobs.py:1974` — `signal.signal(signal.SIGTERM, _handle_sigterm)` — SIGINT is conspicuously not registered here, confirmed experimentally above.
- `models/trial/paths.py:79-121` — `TrialPaths` docstring: on-disk layout (`config.json`, `lock.json`, `results.json` per the docstring — observed on disk as `result.json`, a naming mismatch between the docstring and the actual file worth flagging if precision matters elsewhere).
- `models/job/lock.py:33-79` — `lock.json`'s actual purpose: a reproducibility record (resolved task digest, harbor version, retry policy), **not** a runtime mutex — confirmed by reading its content (`schema_version`, `created_at`, `harbor.version`, per-trial `task.digest`) rather than assuming from the filename.
- `trial/regrade.py:1-13` — the "reconstruct rather than replay" mechanism already in harbor: re-verify a recorded trial's artifacts without re-running the agent, gated on the source trial's artifact manifest containing what the new verifier needs.

## Not determined / out of scope

- Whether a `verifier_environment_mode="separate"` task (e.g. the provisioned `sep-baked`) leaves
  a *second*, separately-named container/network orphaned on a hard crash. Not tested — all four
  cases here use the default shared-verifier mode. Flagging rather than guessing.
- Whether the in-container process the agent-timeout path leaves behind is killed at the moment
  `AgentTimeoutError` is raised or only later when normal teardown runs — noted as a source-level
  inference in Case 4, not directly observed.
