# Native workspace isolation guarantees and execution backends

**Recommendation:** the initial local backend should promise only **accidental
workspace-write non-interference**. It is a fresh-workspace allocator, not a
sandbox. Filesystem confinement, read isolation, credential isolation, process
isolation, network isolation, and hostile-code containment are separate claims
which a selected execution backend may make only when it enforces and records
them. Do not add `sandbox:`, `network:`, or directory-policy keys to workflow
YAML.

This assessment checked Tally at
[`de67a26`](https://github.com/valbaudo/tally/tree/de67a26febfaf6ceb3f279c30614cb4d47beb2fc)
and AWF at
[`860ca17`](https://github.com/valbaudo/awf/tree/860ca172c13c7a86b679db4993acbcd32f4f7cc0).

## Exact local-backend claims

| Claim — use this name | Initial local backend | Evidence and boundary |
| --- | --- | --- |
| **Accidental workspace-write non-interference** | **Yes** | A workspace invocation materializes its input into a fresh `os.MkdirTemp` directory, has no caller-provided `Dir`, and uses that directory as `cmd.Dir`. [`MkdirTemp`](https://pkg.go.dev/os#MkdirTemp) creates a random, `0o700` directory and avoids concurrent allocation collisions; Tally also rejects ambiguous workspace inputs before invocation. [Tally workspace backend](https://github.com/valbaudo/tally/blob/de67a26febfaf6ceb3f279c30614cb4d47beb2fc/backend/claude/workspace.go#L31-L45), [load validation](https://github.com/valbaudo/tally/blob/de67a26febfaf6ceb3f279c30614cb4d47beb2fc/plan/plan.go#L284-L290). This prevents two normal Tally leaves from being handed the same mutable tree; it does not stop code running as the same user from naming another path. |
| **Filesystem write confinement** | **No** | `Cmd.Dir` sets a working directory; it is not an access-control boundary. The current workspace adapter invokes Claude with `--dangerously-skip-permissions`, so an absolute path, `..`, or child process can write any path permitted to the invoking user. [Source](https://github.com/valbaudo/tally/blob/de67a26febfaf6ceb3f279c30614cb4d47beb2fc/backend/claude/workspace.go#L126-L134), [specification](https://github.com/valbaudo/tally/blob/de67a26febfaf6ceb3f279c30614cb4d47beb2fc/SPEC.md#L478-L487). |
| **Read isolation** | **No** | The child has the host user's normal read authority. A captured absolute symlink can also be re-materialized into a downstream workspace. [Tally specification](https://github.com/valbaudo/tally/blob/de67a26febfaf6ceb3f279c30614cb4d47beb2fc/SPEC.md#L478-L486). |
| **Credential isolation** | **No** | Tally does not set `Cmd.Env` for the workspace adapter; Go therefore inherits the current process environment. Host homes and agent configuration are shared ambient state. [`exec.Cmd.Env`](https://pkg.go.dev/os/exec#Cmd), [workspace launch](https://github.com/valbaudo/tally/blob/de67a26febfaf6ceb3f279c30614cb4d47beb2fc/backend/claude/workspace.go#L126-L132). |
| **Process isolation** | **No** | On Unix Tally creates a process group and kills it on cancellation. That is bounded process-tree cleanup, not a private PID namespace or a restriction on seeing/signalling host processes; a new session can escape the group. [Process manager](https://github.com/valbaudo/tally/blob/de67a26febfaf6ceb3f279c30614cb4d47beb2fc/proc/proc.go#L1-L70). |
| **Network isolation** | **No** | There is no network namespace, firewall, proxy, or egress policy in the local launch. Host ports are explicitly named as shared ambient state. [Tally specification](https://github.com/valbaudo/tally/blob/de67a26febfaf6ceb3f279c30614cb4d47beb2fc/SPEC.md#L483-L487). |
| **Hostile-code containment** | **No** | This is the accepted initial decision. Do not call the local backend “sandboxed,” “contained,” or safe for hostile code. It is suitable only for code and host authority the operator chose to run. |

The first row is a data-flow/allocation guarantee. The other rows are security
properties and must never be inferred from the presence of a `workspace`, a
working directory, or a process group.

## Backend boundary

| Mechanism | What it can establish when correctly configured | Why it is not an initial-local guarantee |
| --- | --- | --- |
| Separate workdirs | Only accidental workspace-write non-interference. | A same-user process can still use absolute paths, inherited credentials, the host network, and other processes. |
| macOS `sandbox-exec` / Seatbelt | A profile can restrict resource acquisition; descendant processes inherit it. The installed Apple SDK marks `sandbox-exec(1)` deprecated, while Apple's supported [App Sandbox](https://developer.apple.com/documentation/xcode/configuring-the-macos-app-sandbox) is entitlement-based. Seatbelt also does not restrict already-open file descriptors. | It is macOS-specific, profile-dependent, deprecated, and is not a PID/VM boundary. A profile which permits broad reads or credentials has not supplied read/credential isolation. |
| [Bubblewrap](https://github.com/containers/bubblewrap#sandboxing) | A new mount namespace and selected filesystem visibility; optionally user, IPC, PID, network and UTS namespaces plus seccomp. A private network namespace has loopback only. | Its own documentation says protection is determined by the arguments, and mounted resources (for example a D-Bus socket) can reintroduce authority. It needs a complete policy and a successful launch probe. |
| [Landlock](https://docs.kernel.org/userspace-api/landlock.html) | Linux filesystem allowlists, and on sufficiently new kernels limited TCP/UDP-port and IPC restrictions; policy inherits to descendants. | Kernel ABI support varies; it is not a PID namespace, and files/directories opened before restriction are unaffected. It must be treated as an explicitly tested Linux backend policy. |
| [OCI containers](https://github.com/opencontainers/runtime-spec/blob/main/config-linux.md#namespaces) or VMs | Containers can use mount, PID, network, IPC, user and cgroup namespaces, LSMs and resource limits; a hardened VM/microVM can form a stronger host boundary. [Firecracker's design](https://github.com/firecracker-microvm/firecracker/blob/main/docs/design.md) is a concrete example that treats guest execution as adversarial and layers KVM, a jailer, seccomp, namespaces, cgroups, and dropped privileges. | OCI says an omitted namespace is inherited from the runtime. A container/VM label says nothing about host mounts, privileges, egress, credential injection, or the actual provider configuration. |
| [Cloudflare Computer](https://github.com/cloudflare/computer/blob/main/README.md) | A preview, pluggable execution surface: its Container backend offers full Linux userland and real network, while isolate backends have different semantics. Cloudflare's adjacent [Sandbox SDK](https://developers.cloudflare.com/sandbox/concepts/security/) documents a separate VM per sandbox with filesystem/process/network separation. | Computer itself is not one universal containment contract; its README labels it preview. A Cloudflare adapter must name the selected runtime and policy. In particular, Container internet access is enabled by default unless configured otherwise, and secrets passed into a sandbox remain readable by code in it. [Outbound traffic](https://developers.cloudflare.com/containers/platform-details/outbound-traffic/), [secret handling](https://developers.cloudflare.com/sandbox/concepts/security/#secrets-management). |

AWF demonstrates why this belongs below the language boundary rather than in
Tally YAML. Its current native backend contains OS-specific launchers, chooses
and functionally probes `bwrap`, falls back to a Landlock trampoline, and
records the effective mode; its macOS SBPL profile deliberately supplies
write-only confinement while allowing host reads. [AWF launcher
source](https://github.com/valbaudo/awf/blob/860ca172c13c7a86b679db4993acbcd32f4f7cc0/container/native/sandbox_linux.go#L15-L249),
[macOS profile](https://github.com/valbaudo/awf/blob/860ca172c13c7a86b679db4993acbcd32f4f7cc0/container/native/sandbox_darwin.go#L43-L185),
[documented mode/fallback](https://github.com/valbaudo/awf/blob/860ca172c13c7a86b679db4993acbcd32f4f7cc0/man/awf-workflow.5.md#L273-L323).
That is a useful backend implementation, not a portable language primitive to
copy into Tally.

## Required validation and documentation

1. **Language/preflight:** a local writable-workspace leaf gets one unambiguous
   workspace input and its backend owns the allocation; reject multiple
   writable workspace bindings and caller-selected scratch directories. This
   preserves the current source invariant without exposing an isolation knob.
2. **Runtime:** allocate a new workspace for every invocation/repair attempt,
   materialize immutable input rather than reusing a live directory, and test
   concurrent leaves with separate sentinels plus an unchanged source tree.
3. **Stronger backends:** a backend may advertise a stronger property only
   after a functional enforcement probe, not merely finding `bwrap` or Docker
   on `PATH`. If a selected policy requires a property it cannot establish,
   fail before the leaf runs; never silently degrade to local host authority.
4. **Trace:** persist the selected backend, effective isolation mode/policy
   digest, credential-delivery mode, and egress policy in the run record.
   This mirrors AWF's useful distinction between a requested and effective
   sandbox mode without making the policy part of workflow identity.
5. **Credentials:** call a backend credential-isolated only when it passes an
   explicit allowlist/short-lived capability and does not mount or inherit the
   host credential source. Filesystem or VM isolation alone is insufficient.

Recommended user-facing wording:

> The local backend creates a fresh workspace for each invocation. This keeps
> concurrent Tally steps from accidentally sharing a working tree. It is not a
> sandbox: code runs with the invoking user's host authority and may access
> host files, credentials, processes, and network resources that user can
> access. Backends with stronger isolation document and record their effective
> policy separately.

This keeps the accepted initial boundary intact: **no hostile-code-containment
claim for local execution**. A later adapter can offer a deliberately tested
container/VM or OS-sandbox profile, but the workflow language remains portable
and does not manage Docker infrastructure.
