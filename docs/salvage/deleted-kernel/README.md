# Design records for deleted work

Everything here specifies systems that no longer exist: the canonical workflow kernel,
the structured control runtime, the typed value/contract system, the workspace kernel,
the agent-adapter capability boundary, the YAML plan language.

Kept as a record of the reasoning, not as current specification. Nothing here is being
built. The current design is `docs/GLUE-DESIGN.md`.

The short version of why it went: the kernel typed the payload and left the envelope
empty, and it owned control flow, which every high-scoring harness writes as a `for` loop.
Its central `LeafRunner` port had four test implementations and zero real ones.
