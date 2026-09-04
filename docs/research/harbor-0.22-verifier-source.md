# Harbor 0.22.0 verifier internals — source-code findings

- **Harbor version (confirmed via installed dist-info metadata):** `0.22.0`
- **Source root read:**
  `/Users/vabbb/.local/share/uv/tools/harbor/lib/python3.14/site-packages/harbor/`
- **Method:** direct `grep`/`sed`/`Read` of the installed package source. No web search, no secondary write-ups, no version other than the one installed locally.

All paths below are given relative to the source root above unless a full path is shown. Line numbers refer to the file as installed; a short verbatim quote follows every citation.

---

## 1. Reward resolution precedence (`reward.txt` vs `reward.json`)

The check happens in `verifier/verifier.py`, inside `Verifier.verify()`.

**`verifier/verifier.py:227-236`**
```python
if self.trial_paths.reward_json_path.exists():
    rewards = self._parse_reward_json()
elif self.trial_paths.reward_text_path.exists():
    rewards = self._parse_reward_text()
else:
    raise RewardFileNotFoundError(
        f"No reward file found at {self.trial_paths.reward_text_path} or {
            self.trial_paths.reward_json_path
        }"
    )
```

**Finding: `reward.json` IS checked first, and it DOES preempt `reward.txt` when both exist.** The `if` branch tests `reward_json_path.exists()` before the `elif` branch even looks at `reward_text_path`. If a trial's verifier writes both `/logs/verifier/reward.json` and `/logs/verifier/reward.txt`, only `reward.json` is parsed (via `_parse_reward_json`, `verifier/verifier.py:81-94`); `reward.txt` is silently ignored. `reward.txt` is only consulted when `reward.json` does not exist at all.

This confirms the previous reading's claim ("`reward.json` is read before `reward.txt`") is **TRUE** in 0.22.0 — there is no contradiction to flag here.

Supporting parser code:

**`verifier/verifier.py:66-79`** (`_parse_reward_text`)
```python
def _parse_reward_text(self) -> dict[str, float | int]:
    if self.trial_paths.reward_text_path.stat().st_size == 0:
        raise RewardFileEmptyError(
            f"Reward file is empty at {self.trial_paths.reward_text_path}"
        )
    try:
        return {"reward": float(self.trial_paths.reward_text_path.read_text())}
```

**`verifier/verifier.py:81-94`** (`_parse_reward_json`)
```python
def _parse_reward_json(self) -> dict[str, float | int]:
    if self.trial_paths.reward_json_path.stat().st_size == 0:
        raise RewardFileEmptyError(
            f"Reward file is empty at {self.trial_paths.reward_json_path}"
        )
    try:
        return json.loads(self.trial_paths.reward_json_path.read_text())
```

Path definitions (host side, `models/trial/paths.py:272-285`):
```python
@property
def reward_text_path(self) -> Path:
    """
    A text file containing the float reward. Alternative to the JSON file.
    """
    return self.verifier_dir / "reward.txt"

@property
def reward_json_path(self) -> Path:
    """
    A flat JSON file containing key-value pairs for each reward. Alternative to
    the text file.
    """
    return self.verifier_dir / "reward.json"
```

No other code path in the installed package re-implements this precedence differently — `grep -rn "reward.txt\|reward.json"` across the package turns up only this one authoritative resolution site in `verifier/verifier.py` (other hits are in CLI viewers/adapters that just read whichever file is present for display, or in path constant declarations).

---

## 2. Verifier `environment_mode` — declared default and runtime resolution

### Declared model default

**`models/task/config.py:554-558`** (the enum)
```python
class VerifierEnvironmentMode(str, Enum):
    """Whether the verifier runs in the agent's environment or its own."""

    SHARED = "shared"
    SEPARATE = "separate"
```

**`models/task/config.py:561,568-577`** (the field on `VerifierConfig`)
```python
class VerifierConfig(PhaseNetworkPolicyConfig):
    ...
    environment_mode: VerifierEnvironmentMode | None = Field(
        default=None,
        description=(
            "Whether the verifier runs in the agent's environment ('shared') "
            "or in a dedicated container ('separate'). When omitted: defaults "
            "to 'separate' if a verifier 'environment' is set, otherwise "
            "'shared'."
        ),
    )
```

**The declared Pydantic default is `None`, not `"shared"`.** The field is optional and unset by default; "shared" is only what it *resolves to* at runtime absent other signals (see below).

### Runtime resolution logic

**`models/task/verifier_mode.py:10-26`**
```python
def _resolve_mode(verifier: VerifierConfig) -> VerifierEnvironmentMode | None:
    """Effective mode for a single VerifierConfig, before inheritance.

    Returns the explicit mode when set, otherwise infers SEPARATE from the
    presence of an environment definition, otherwise None to signal "not
    specified at this level."
    """
    if verifier.environment_mode is not None:
        return verifier.environment_mode
    if verifier.environment is not None:
        return VerifierEnvironmentMode.SEPARATE
    return None


def resolve_task_verifier_mode(task_cfg: TaskConfig) -> VerifierEnvironmentMode:
    """Resolve the trial-level verifier mode for a single-step task."""
    return _resolve_mode(task_cfg.verifier) or VerifierEnvironmentMode.SHARED
```

So the resolution chain is: explicit `environment_mode` wins → else presence of `[verifier.environment]` implies `SEPARATE` → else the final fallback is `SHARED`. **`SHARED` is the effective default only as the last fallback of `resolve_task_verifier_mode`/`resolve_step_verifier_mode`, not as the Pydantic field default itself.**

### What "shared" means at runtime — does the verifier execute in the agent's own container?

Dispatch in `trial/single_step.py:98-108`:
```python
if mode == VerifierEnvironmentMode.SEPARATE:
    self.result.verifier_result = await self._run_separate_verifier(
        key="trial",
        timeout_sec=self._verifier_timeout_sec,
        artifacts_dir=self.paths.artifacts_dir,
        user=user,
    )
else:
    self.result.verifier_result = await self._run_shared_verifier(
        timeout_sec=self._verifier_timeout_sec,
        user=user,
    )
```

`_run_shared_verifier` (`trial/trial.py:641-667`) constructs the `Verifier` with `environment=self.agent_environment` — literally the same `BaseEnvironment` object the agent phase ran in:
```python
async def _run_shared_verifier(
    self,
    *,
    timeout_sec: float | None,
    user: str | int | None,
    env: dict[str, str] | None = None,
    step_name: str | None = None,
    step_cfg: StepConfig | None = None,
) -> VerifierResult:
    plan = self._network_plan(step_cfg)
    with self.agent_environment.with_default_user(user):
        verifier = VerifierFactory.create_verifier_from_config(
            self.config.verifier,
            task=self.task,
            trial_paths=self.paths,
            environment=self.agent_environment,
            ...
```

**Confirmed: `shared` mode means the verifier's test script is executed by `environment.exec(...)` inside the exact same running container the agent used** (no new container is created; `_run_separate_verifier`, by contrast, calls `EnvironmentFactory.create_environment_from_config(...)` to build a brand-new environment — see §3).

---

## 3. `[verifier.environment]` override — does it force separate mode?

**Yes — setting `[verifier.environment]` implicitly forces `environment_mode='separate'`, without needing `environment_mode` to be set explicitly.** This is exactly the second branch of `_resolve_mode` quoted in §2:

**`models/task/verifier_mode.py:17-20`**
```python
if verifier.environment_mode is not None:
    return verifier.environment_mode
if verifier.environment is not None:
    return VerifierEnvironmentMode.SEPARATE
```

The field docstring states this explicitly too (`models/task/config.py:579-587`):
```python
environment: EnvironmentConfig | None = Field(
    default=None,
    description=(
        "Environment definition for the separate verifier container. "
        "Same schema as the top-level [environment] section. When set "
        "without an explicit environment_mode, implies "
        "environment_mode='separate'. When unset with "
        "environment_mode='separate', a fresh copy of the top-level "
        "[environment] is used. Conflicts with "
        "environment_mode='shared'."
    ),
)
```

The reverse combination is explicitly rejected by a validator — you cannot set `environment_mode='shared'` together with `[verifier.environment]`:

**`models/task/config.py:600-609`**
```python
@model_validator(mode="after")
def _validate_mode_env_consistency(self) -> "VerifierConfig":
    if (
        self.environment_mode == VerifierEnvironmentMode.SHARED
        and self.environment is not None
    ):
        raise ValueError(
            "[verifier].environment_mode='shared' is incompatible with "
            "[verifier.environment]; either omit the environment or set "
            "environment_mode='separate'."
        )
    return self
```

### Where the separate verifier environment is actually constructed

`resolve_effective_verifier_env_config` picks the environment definition to use (step-level override > task-level `[verifier.environment]` > a fresh deep copy of the top-level `[environment]`):

**`models/task/verifier_mode.py:43-64`**
```python
def resolve_effective_verifier_env_config(
    task_cfg: TaskConfig, step_cfg: StepConfig | None
) -> EnvironmentConfig | None:
    """Return the EnvironmentConfig the verifier should run in, or None for shared.

    Lookup order for the env definition when the effective mode is SEPARATE:
    step.verifier.environment > task.verifier.environment > a fresh deep copy
    of task.environment.
    """
    ...
    if step_cfg is not None and step_cfg.verifier.environment is not None:
        return step_cfg.verifier.environment
    if task_cfg.verifier.environment is not None:
        return task_cfg.verifier.environment
    return task_cfg.environment.model_copy(deep=True)
```

The new container is actually built in `_run_separate_verifier` / `_separate_verifier_env` (`trial/trial.py:674-782`), which calls `EnvironmentFactory.create_environment_from_config(...)` (a fresh environment, not the agent's), e.g.:

**`trial/trial.py:759-772`**
```python
env = EnvironmentFactory.create_environment_from_config(
    config=verifier_runtime_config,
    environment_dir=self._verifier_env_build_context(step_cfg),
    environment_name=self.task.short_name,
    session_id=self._separate_verifier_session_id(key),
    trial_paths=self.paths,
    task_env_config=env_config,
    logger=self.logger,
    mounts=self._verifier_env_mounts(env_config),
    network_policy=plan.verifier_env_baseline,
    phase_network_policies=[plan.verifier_phase],
)
```

---

## 4. Network policy — per-phase application, legal values, agent vs verifier divergence

### Legal `network_mode` values

**`models/task/config.py:36-41`**
```python
class NetworkMode(str, Enum):
    """Network access policy for agent and verifier execution."""

    NO_NETWORK = "no-network"
    PUBLIC = "public"
    ALLOWLIST = "allowlist"
```

### Where it is applied per phase

Both `[agent]` and `[verifier]` sections carry an *optional* phase-override `network_mode` field via a shared mixin:

**`models/task/config.py:218-231,237-241`**
```python
class PhaseNetworkPolicyConfig(AllowedHostsValidationMixin, BaseModel):
    """Network policy fields for [agent] and [verifier] phase overrides."""

    network_mode: NetworkMode | None = Field(
        default=None,
        description="Network access policy. [agent] and [verifier] use this only "
        "as an explicit phase override when set.",
    )
    ...
    def explicit_phase_policy(self) -> NetworkPolicy | None:
        if self.network_mode is None:
            return None
        return NetworkPolicy(
            network_mode=self.network_mode,
            allowed_hosts=list(self.allowed_hosts or []),
        )
```
`AgentConfig` (`models/task/config.py:339`) and `VerifierConfig` (`models/task/config.py:561`) both subclass `PhaseNetworkPolicyConfig`.

The actual per-phase resolution and application lives in `trial/network_policy.py` and is applied in `trial/trial.py`.

**`trial/network_policy.py:105-131`**
```python
def resolve_agent_phase_policy(
    task_cfg: TaskConfig,
    trial_agent_cfg: TrialAgentConfig,
    agent_env_baseline: NetworkPolicy,
    step_cfg: StepConfig | None = None,
) -> NetworkPolicy:
    """Effective agent policy during agent.run()."""
    explicit = _explicit_phase_policy(task_cfg, step_cfg, "agent")
    extra_hosts = normalize_allowed_hosts(list(trial_agent_cfg.extra_allowed_hosts))

    policy = explicit or agent_env_baseline
    if extra_hosts:
        policy = merge_extra_allowlists(policy, extra_hosts)
    return policy


def resolve_verifier_phase_policy(
    task_cfg: TaskConfig,
    step_cfg: StepConfig | None = None,
    *,
    baseline: NetworkPolicy,
) -> NetworkPolicy:
    """Effective verifier policy during verify()."""
    explicit = _explicit_phase_policy(task_cfg, step_cfg, "verifier")
    if explicit is None:
        return baseline
    return explicit
```

At execution time, `trial/trial.py`'s `_phase_network_policy` async context manager actually switches the live network policy in and out around each phase:

**`trial/trial.py:262-278`**
```python
@contextlib.asynccontextmanager
async def _phase_network_policy(
    self,
    environment: BaseEnvironment,
    *,
    baseline_policy: NetworkPolicy,
    phase_policy: NetworkPolicy,
) -> AsyncGenerator[None, None]:
    if phase_policy == baseline_policy:
        yield
        return

    await environment.set_network_policy(phase_policy)
    try:
        yield
    finally:
        await environment.set_network_policy(baseline_policy)
```
This is invoked around `verifier.verify()` in both `_run_shared_verifier` (`trial/trial.py:659-666`) and `_run_separate_verifier` (`trial/trial.py:733-739`).

### Can the verifier phase have a different network policy than the agent phase?

**Yes, in both shared and separate verifier modes.** `TrialNetworkPlan` (`trial/network_policy.py:134-144`) explicitly tracks independent `agent_phase` and `verifier_phase` policies:
```python
@dataclass(frozen=True)
class TrialNetworkPlan:
    agent_env_baseline: NetworkPolicy
    agent_phase: NetworkPolicy
    verifier_env_baseline: NetworkPolicy | None
    verifier_phase: NetworkPolicy
```
and `resolve_trial_network_plan` (`trial/network_policy.py:147-191`) resolves `agent_phase` and `verifier_phase` independently — `verifier_phase` falls back to `agent_env_baseline` only when in shared mode and no explicit `[verifier]` override exists (`trial/network_policy.py:164-166`), otherwise it can be set independently via `[verifier].network_mode`/`allowed_hosts`, or via a wholly separate verifier environment's own baseline.

Caveat: dynamically switching network policy mid-container-lifetime is only supported by environments whose capability flag says so. If a phase's resolved policy differs from the environment's baseline, Harbor validates that the environment supports it and raises if not:

**`trial/trial.py:226-238`**
```python
def _validate_dynamic_phase_switch(
    self,
    environment: BaseEnvironment,
    *,
    phase: NetworkPolicy,
    phase_label: str,
    environment_label: str,
) -> None:
    environment.validate_network_policy_support(phase)
    if not environment.capabilities.dynamic_network_policy:
        raise ValueError(
            f"{phase_label} network policy differs from the {environment_label} "
            "baseline, but this environment cannot change network policy after "
            "start."
        )
```

---

## 5. `/logs` and `/logs/verifier` — creation and mount

### Host-side directory creation

**`models/trial/paths.py:148-151`** (`TrialPaths.mkdir`)
```python
def mkdir(self):
    self.agent_dir.mkdir(parents=True, exist_ok=True)
    self.verifier_dir.mkdir(parents=True, exist_ok=True)
    self.artifacts_dir.mkdir(parents=True, exist_ok=True)
```
Called from `Trial.__init__` at **`trial/trial.py:120`**: `self.paths.mkdir()`.

Permissions are opened up for host-mounted providers only:

**`models/trial/paths.py:153-160`**
```python
def chmod_dir(self):
    """Set permissions for agent, verifier, and artifacts dirs."""
    self.trial_dir.chmod(0o777)
    self.agent_dir.chmod(0o777)
    if self.user_agent_dir.exists():
        self.user_agent_dir.chmod(0o777)
    self.verifier_dir.chmod(0o777)
    self.artifacts_dir.chmod(0o777)
```
Called conditionally in `trial/trial.py:1113-1115`:
```python
if self.agent_environment.capabilities.mounted:
    self.paths.chmod_dir()
    self._chmod_artifact_mount_chain()
```

### Container-side mount (bind mount from the host trial directory)

The mount list for the **agent's own environment** (the one the agent phase runs in) explicitly bind-mounts `verifier_dir` and `agent_dir` from the host trial directory into the container **at agent-environment creation time**, i.e. before the agent phase even starts:

**`trial/trial.py:1629-1662`** (`_agent_env_mounts` property)
```python
@property
def _agent_env_mounts(self) -> list[ServiceVolumeConfig]:
    base: list[ServiceVolumeConfig] = [
        ServiceVolumeConfig(
            type="bind",
            source=self.paths.verifier_dir.resolve().absolute().as_posix(),
            target=str(self.agent_env_paths.verifier_dir),
        ),
        ServiceVolumeConfig(
            type="bind",
            source=self.paths.agent_dir.resolve().absolute().as_posix(),
            target=str(self.agent_env_paths.agent_dir),
        ),
    ]
    ...
```
This list is passed to `EnvironmentFactory.create_environment_from_config(..., mounts=self._agent_env_mounts, ...)` at `trial/trial.py:1110` when the agent environment is created (`_init_agent_environment`, `trial/trial.py:1087-1116`).

`EnvironmentPaths` (container-side target paths, `models/trial/paths.py:37-41`):
```python
logs_dir: PurePosixPath = PurePosixPath("/logs")
agent_dir: PurePosixPath = logs_dir / "agent"
user_agent_dir: PurePosixPath = logs_dir / "user-agent"
verifier_dir: PurePosixPath = logs_dir / "verifier"
artifacts_dir: PurePosixPath = logs_dir / "artifacts"
```

**Is `/logs/verifier` writable by the agent during the agent phase?** For host-mounted providers (`capabilities.mounted=True`), **yes** — it's a genuine bind mount (`type="bind"`) present in the container from the moment the agent's environment is created (before the agent's instruction ever runs), and `chmod_dir()` sets it to `0o777`. Nothing in the agent-phase code path removes or hides this mount, so it is present and world-writable inside the container throughout the agent phase, not just during verification.

For **non-mounted** providers (`capabilities.mounted=False`, e.g. cloud sandbox providers), there is no host bind mount at all — Harbor instead downloads the verifier directory from the environment after the verifier runs:

**`verifier/verifier.py:204-225`**
```python
if not self.environment.capabilities.mounted:
    try:
        if self.include_logs or self.exclude_logs:
            await self.environment.download_dir_filtered(
                source_dir=str(env_paths.verifier_dir),
                target_dir=self.trial_paths.verifier_dir,
                ...
            )
        else:
            await self.environment.download_dir(
                source_dir=str(env_paths.verifier_dir),
                target_dir=self.trial_paths.verifier_dir,
            )
```

The local Docker provider sets `mounted=True` explicitly:

**`environments/docker/docker.py:290-305`**
```python
@override
def capabilities(self) -> EnvironmentCapabilities:
    return EnvironmentCapabilities(
        ...
        windows=True,
        mounted=True,
        docker_compose=True,
    )
```
vs. the base-class default:

**`environments/capabilities.py:53-55`**
```python
mounted: bool = False
"""Whether the environment mounts log directories as host filesystems."""
```

The **separate verifier** container gets its own, narrower bind mount — only `/logs/verifier`, from the same host directory:

**`trial/trial.py:786-796`**
```python
def _verifier_env_mounts(
    self,
    env_config: EnvironmentConfig,
) -> list[ServiceVolumeConfig]:
    env_paths = EnvironmentPaths.for_os(env_config.os)
    return [
        ServiceVolumeConfig(
            type="bind",
            source=self.paths.verifier_dir.resolve().absolute().as_posix(),
            target=str(env_paths.verifier_dir),
        )
    ]
```

---

## 6. Artifact export — `[verifier] collect` vs top-level `artifacts = []`

These are two distinct mechanisms:

### `[[verifier.collect]]` — pre-collection snapshot commands

Declared on `VerifierConfig`:

**`models/task/config.py:588-598`**
```python
collect: list["VerifierCollectConfig"] = Field(
    default_factory=list,
    description=(
        "Commands run in compose services after the agent phase ends and "
        "before artifact collection ([[verifier.collect]] blocks in "
        "task.toml). Use these to snapshot runtime state into files that "
        "artifact entries can then collect."
    ),
)
```

`VerifierCollectConfig` itself:

**`models/task/config.py:725-745`**
```python
class VerifierCollectConfig(BaseModel):
    """A command run inside a compose service after the agent phase ends.

    Collect hooks let services snapshot runtime state (database contents,
    in-memory counters) into files before the environment is torn down, so
    the files can be declared as artifacts and read by a separate verifier.
    Hooks targeting the main service run before the main container is
    stopped; hooks targeting sidecars run after it is stopped.
    """

    command: str = Field(..., description="Shell command to run in the service.")
    service: str = Field(
        default=MAIN_SERVICE_NAME,
        description="Compose service to run the command in. Defaults to main.",
    )
    timeout_sec: float = Field(
        default=60.0,
        description="Timeout in seconds for the collect command.",
    )
```

Execution (`trial/trial.py:1214-1241`, `_run_collect_hooks`) runs each hook's `command` via `self.agent_environment.service_exec(...)`, **best-effort** — failures are logged as warnings and never abort the trial:
```python
async def _run_collect_hooks(
    self,
    hooks: Sequence[VerifierCollectConfig],
) -> None:
    """Run collect hooks best-effort; failures never abort the trial."""
    for hook in hooks:
        ...
        try:
            result = await self.agent_environment.service_exec(
                hook.command,
                service=hook.service,
                timeout_sec=int(hook.timeout_sec),
                user=hook.user,
            )
```
These hooks run inside `_collect_artifacts_phased` (`trial/trial.py:1246-1300`), which runs `main_hooks` before downloading main-service artifacts, and `sidecar_hooks` before downloading sidecar-service artifacts — i.e. `collect` produces files that the `artifacts` mechanism (below) can then pick up.

### Top-level `artifacts = []` in `task.toml` — what actually gets downloaded to the host

Declared directly on `TaskConfig` (root of `task.toml`, not nested under `[verifier]`):

**`models/task/config.py:795,820`**
```python
class TaskConfig(BaseModel):
    ...
    artifacts: list[str | ArtifactConfig] = Field(default_factory=list)
```

This list (plus any trial-level `config.artifacts` and, for multi-step tasks, per-step `StepConfig.artifacts`, `models/task/config.py:780-785`) is merged and handed to an `ArtifactHandler`:

**`trial/trial.py:1137-1142`**
```python
def _init_artifact_handler(self) -> None:
    self._validate_artifact_configuration()
    self._artifact_handler = ArtifactHandler(
        artifacts=[*self.task.config.artifacts, *self.config.artifacts],
        logger=self.logger,
    )
```

`ArtifactHandler.download_artifacts` (`trial/artifact_handler.py:173-206`) is what actually copies files out of the environment onto the host, called from `_collect_artifacts_phased` (`trial/trial.py:1271-1279`, `1300-1306`):
```python
await self._artifact_handler.download_artifacts(
    self.agent_environment,
    artifacts_dir,
    source_artifacts_dir=self.agent_env_paths.artifacts_dir,
    artifacts=step_artifacts,
    services={MAIN_SERVICE_NAME},
)
```

### Where collected files land on the host

`artifacts_dir` passed in is `self.paths.artifacts_dir` (single-step trials, `trial/single_step.py` via `_collect_artifacts`), which resolves to:

**`models/trial/paths.py:214-222`**
```python
@property
def artifacts_dir(self) -> Path:
    """
    A directory for collected artifacts from the environment.

    Contains files downloaded from the convention directory (/logs/artifacts/)
    and any config-driven artifact paths.
    """
    return self.trial_dir / "artifacts"
```

i.e. `<trial_dir>/artifacts/`, mirrored from each artifact's absolute container source path (or an explicit `destination`), with a `manifest.json` written by `ArtifactHandler` (`trial/artifact_handler.py:_write_manifest`, referenced in the class docstring at `trial/artifact_handler.py:44-58`) listing what was collected from where.

---

## 7. Default `max_retries`

**`models/job/config.py:291-293`**
```python
class RetryConfig(BaseModel):
    max_retries: int = Field(
        default=0, description="Maximum number of retry attempts", ge=0
    )
```

**Confirmed: the default value of `max_retries` is `0`** (no automatic retries unless a job explicitly configures a higher value). It is consumed in the retry loop at `trial/queue.py:200,215`:
```python
for attempt in range(self._retry_config.max_retries + 1):
    ...
    if attempt == self._retry_config.max_retries:
```

Note also that `RetryConfig` carries a default `exclude_exceptions` set that specifically excludes verifier-related failures from being retried even if `max_retries` were raised above 0 (`models/job/config.py:300-310`):
```python
exclude_exceptions: set[str] | None = Field(
    default_factory=lambda: {
        "AgentTimeoutError",
        "VerifierTimeoutError",
        "RewardFileNotFoundError",
        "RewardFileEmptyError",
        "VerifierOutputParseError",
        "ApiUsageLimitError",
        "AgentSafetyRefusalError",
        "AgentAuthenticationError",
        "ModelNotFoundError",
    },
    ...
)
```
This wasn't asked directly but is directly adjacent and load-bearing for anyone reasoning about verifier-failure retry behavior, so it is included here rather than left out.

---

## Nothing marked "NOT FOUND"

Every question above was answered directly from source with a citation; no claim in this document required guessing or inference beyond straightforward code reading.
