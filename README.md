# dawn

Wiped to the foundation on 2026-09-02. What remains is the load-bearing 1.6k lines;
the 13.6k lines of workflow IR, scheduler, typed value system, YAML plan language and
LLM-jury gate were deleted.

Why: the old design typed the payload and left the envelope empty. It owned control
flow, which every high-scoring agent harness writes as ordinary code, and recorded none
of cost, timing, attempt history or execution position — the things every harness has
to build by hand.

| Path | What | Deps |
|------|------|------|
| `store/` | content-addressed blobs and tree manifests; symlinks round-trip, exec bit preserved, no shelling out | none |
| `proc/` | child processes; on Unix the whole group dies on cancel, with a bounded pipe-wait fallback | none |
| `lock/` | non-blocking flock on a state directory, so a cron that overruns its interval is told it is late | none |
| `platform.go` | macOS and Linux only, enforced at compile time | none |

No third-party dependencies.

- The design being built: [docs/GLUE-DESIGN.md](docs/GLUE-DESIGN.md)
- Reasoning salvaged from the deleted code: [docs/salvage/dawn-reasoning.md](docs/salvage/dawn-reasoning.md)
- Everything deleted is recoverable: `git show pre-glue-wipe:<path>`
