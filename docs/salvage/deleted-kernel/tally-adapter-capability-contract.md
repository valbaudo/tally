# Tally adapter capability contract research

Research note for [issue #3](https://github.com/valbaudo/tally/issues/3), 2026-08-08. This is evidence gathering for Wayfinder, not an implementation specification or provider-configuration schema.

## Recommendation

Use one common leaf-result envelope and five small, independently declared capabilities. Do **not** make `files`, `workspace`, or `resume` each a single boolean.

Every adapter returns a candidate result (`text`, optional JSON value, optional artifact handles, usage, and an opaque provider reference). Tally remains the authority that validates the workflow JSON Schema and commits values/artifacts. A node may require the following capabilities:

| Capability | Truthful contract | Why it is separate |
| --- | --- | --- |
| `structured-json` | The adapter accepts a JSON Schema and returns JSON or a typed failure; Tally validates the returned value against the canonical workflow schema. | Native schema enforcement is useful, but OpenAI and Anthropic both support only schema subsets. Provider enforcement is an adapter detail; local validation is still required. |
| `raw-attachments` | The adapter receives named artifact bytes/streams and declares supported MIME patterns plus the fidelity it supplies (`text` or `visual`). | A raw-model attachment is not a workspace. A generic `files: true` would lie about PDFs, images, charts, and office files. |
| `workspace-materialize` | The adapter can run in a fresh workspace root materialized from a Tally tree; implementation is `local-directory` or `remote-files`. | A tool agent needs paths and edit tools, not message attachments. |
| `workspace-capture` | Declared output files, or a declared workspace tree, can be copied into Tally's artifact store before the leaf succeeds. | Materializing a workspace does not prove that the adapter can retrieve its resulting files. |
| `same-node-recovery` | After submission the adapter emits a durable run reference which Tally can checkpoint, poll/retrieve, and cancel. | Continuing a chat transcript is not recovering the interrupted invocation. |

Artifact names, media types, file/tree kind, schemas, and a node's required capabilities belong to the language. Provider file IDs, base64 versus URL upload, container IDs, command flags, models, tools, permissions, and MCP configuration remain adapter-owned.

## Evidence across realistic initial transports

| Transport | Structured output | Inputs and workspace | Interrupted-node conclusion |
| --- | --- | --- | --- |
| **OpenAI Responses (raw model)** | [Structured Outputs](https://developers.openai.com/api/docs/guides/structured-outputs) can constrain a response to a supplied JSON Schema, but supports a documented subset. | [`input_file`](https://developers.openai.com/api/docs/guides/file-inputs) accepts base64, a Files API ID, or a URL. PDFs on vision models carry text and page images; non-PDF documents are text-only and embedded charts/images are not preserved. It has no local workspace contract. | [Background mode](https://developers.openai.com/api/docs/guides/background) creates an async Response and supports polling it by response ID. This is a valid `same-node-recovery` implementation only when Tally durably records that ID before treating the node as running. |
| **Anthropic Messages (raw model)** | [`output_config.format`](https://platform.claude.com/docs/en/build-with-claude/structured-outputs) returns JSON matching a schema, with documented limits. | The [Files API](https://platform.claude.com/docs/en/build-with-claude/files) can reference uploaded PDFs, plain text, and images by file ID; unsupported document types have different semantics (for example, `.docx`/`.xlsx` must be converted for document input). It is an attachment channel, not a workspace. | The cited Messages/Files interface supplies no pollable in-flight run handle; do not advertise `same-node-recovery` from a normal request. |
| **Codex `exec` (local tool agent)** | [`--output-schema`](https://developers.openai.com/codex/noninteractive/) requests a JSON-Schema-shaped final response. | [`--cd`](https://developers.openai.com/codex/cli/reference/) selects the workspace root; the agent's sandbox can read/write inside that workspace. The runtime should stage named artifacts at deterministic paths and snapshot/capture them, rather than assume a provider file API. Direct attachment support is explicitly images (`--image`), not arbitrary files. | [`codex exec resume`](https://developers.openai.com/codex/noninteractive/) continues a previous session with a follow-up prompt. It is not a handle for polling or retrieving the original interrupted turn, so it is **not** `same-node-recovery`. |
| **Claude Code `-p` (local tool agent)** | The [CLI supports `--json-schema`](https://code.claude.com/docs/en/cli-usage) for validated JSON after the agent workflow. | Claude Code can read/edit its working directory and additional directories. Tally's current [`claude-ws`](https://github.com/valbaudo/tally/blob/de67a26febfaf6ceb3f279c30614cb4d47beb2fc/backend/claude/workspace.go#L19-L205) materializes one tree into a fresh directory, runs the CLI there, then captures a new content-addressed tree. This proves the local materialize/capture shape, while also showing why named files must become first-class artifacts rather than one magical workspace field. | The current adapter passes `--no-session-persistence`; it therefore has no recovery handle. [Background Claude Code sessions](https://code.claude.com/docs/en/agent-view) can persist/restart their own work, but a separate adapter may advertise recovery only if it checkpoints that session ID and captures the workspace artifact; this is not a general completed-result guarantee. |
| **OpenAI Code Interpreter (remote tool agent)** | Uses the surrounding Responses structured-output facility. | [Containers](https://developers.openai.com/api/docs/guides/tools-code-interpreter) accept uploaded/generated files and expose output file IDs and filenames; adapters can list/download them. Containers are explicitly ephemeral: inactive containers expire after 20 minutes and their data cannot be recovered. | A remote container ID alone is not durable workflow state. A remote-workspace adapter must download the declared files/tree into Tally storage before commit, and must rematerialize a fresh container after expiry. |

## Required validation and recovery rules

These combinations should be rejected before dispatch, not discovered after spending tokens:

- A declared JSON output requires `structured-json`. The adapter must preflight any provider-specific schema restriction before invocation; Tally still performs the canonical JSON Schema validation after it returns.
- An attachment requirement must match the adapter's advertised MIME pattern and fidelity. A node that needs visual understanding of a chart cannot silently use a text-only document path.
- A node with workspace paths, editable inputs, or declared captured files requires both `workspace-materialize` and the appropriate `workspace-capture` mode. Raw attachment support does not satisfy either requirement.
- A remote workspace that promises a tree or named output files requires enumerate/download capability; a container ID or prose file citation is insufficient.
- A node explicitly requesting same-node recovery requires `same-node-recovery`; ordinary foreground HTTP calls and CLI invocations must instead rerun from Tally's durable input/artifact snapshot.

`same-node-recovery` changes neither commit semantics nor external-effect semantics. Tally commits only after it has captured and validated the terminal result. If recovery is unavailable, an uncommitted node may run again; the runtime's stable idempotency key remains the protection for external effects.

## Ownership boundary

| Owner | Owns | Does not own |
| --- | --- | --- |
| **Language** | Typed values, named immutable artifacts, artifact kind/media, JSON Schema contracts, and required capability names. | Provider IDs, upload method, local paths, container lifecycle, tool or permission configuration. |
| **Runtime** | Content-addressed artifact/tree storage, deterministic staging paths and manifests, schema validation, capture-before-commit, capability checks, durable node/run checkpoints, and idempotency policy. | Docker/container infrastructure or provider-specific file/session semantics. |
| **Adapter** | Capability declaration, schema translation/preflight, provider uploads/downloads, local working-directory launch, remote-container extraction/rematerialization, and opaque run references. | Changing Tally's artifact identity or silently weakening a requested capability. |
| **Provider/agent** | Model-constrained decoding, file-processing fidelity, tool execution, and any transient container/session state. | Durable Tally state and cross-adapter artifact portability. |

The initial implementation can therefore support raw-model attachment roles and local tool-agent workspace roles without pretending that either transport does both. Remote workspaces and pollable async runs fit later behind the same capability vocabulary, with no new workflow syntax.
