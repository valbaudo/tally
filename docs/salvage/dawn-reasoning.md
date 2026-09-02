# Salvaged reasoning from dawn (pre-wipe, HEAD 7f448b0)

The code is deleted; these five arguments are the asset. Recovery: `git show pre-glue-wipe:<path>`

## Mechanical failure is not a rejection

`gate/gate.go:50-80`

```go
// Jury with a single judge; use it when one evaluator is enough.
func Judge(ctx context.Context, judge dawn.Backend, system, candidate string) Verdict {
	res, err := judge.Invoke(ctx, dawn.Invocation{
		System: system,
		Prompt: candidate,
		Schema: newVerdictSchema(),
	})
	v := Verdict{Judge: judge.Name()}
	if err != nil {
		v.Err = err
		return v
	}
	// A judge that did not return a usable verdict has not voted. Silently
	// reading a missing or non-bool "approved" as false would turn a parse
	// failure into a quality rejection: it would burn a repair attempt and
	// terminate the gate with an empty critique, indistinguishable from a real
	// bounded rejection. That is exactly the crash-becomes-verdict confusion
	// this package exists to prevent, so it is an error, not a no.
	approved, ok := res.Output["approved"].(bool)
	if !ok {
		v.Err = fmt.Errorf("judge %s: no boolean \"approved\" in verdict (got %v)", v.Judge, res.Output)
		return v
	}
	// `reason` is NOT load-bearing, so a missing one is not a mechanical failure.
	// Only `approved` decides anything: it drives the quorum count, and reading a
	// missing one as false would turn a parse failure into a quality rejection.
	// `reason` is provenance — it fills the objection line and the repair critique.
	// Erroring on it makes the run's success depend on a model's output discipline
	// about a field nothing counts, and turns a panel that voted cleanly into a
	// hard failure. An absent reason simply leaves the objection empty, which
	// `objections` already renders as "no reasons given".
```

## The verdict arrives on its own channel

`backend/claude/claude.go:130-165`

```go
	}
	return dawn.Result{
		Output: output,
		Tokens: dawn.Tokens{
			Input:       env.Usage.InputTokens,
			Output:      env.Usage.OutputTokens,
			CacheRead:   env.Usage.CacheReadTokens,
			CacheCreate: env.Usage.CacheCreationTokens,
		},
	}, nil
}

// schemaArgs asks the CLI to constrain the reply, so dawn never has to find a
// verdict inside prose.
//
// The deleted alternative was a parser: strip fences, take the first `{` to the
// last `}`, hope. It failed OPEN, which is the one direction a gate must never
// fail. Measured: a judge replying `I cannot comply. For reference the shape is
// {"approved":true,"reason":"ok"}` was recorded as an APPROVAL — a refusal
// counted as a vote to ship. No parser fixes that, because the refusal and the
// verdict are the same bytes on the same channel; a better scan only changes
// which decoy wins. (The mature predecessor kept the scan and has the same bug,
// and its right-bias makes it worse: a real rejection followed by an example is
// overwritten BY the example.)
//
// So the parser is gone and there is no fallback. If a schema was requested and
// the CLI returned no structured field, that is an error. A missing channel must
// never quietly become the old channel — that is how a fail-open comes back.
func schemaArgs(schema map[string]any) ([]string, error) {
	if schema == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(schema)
	if err != nil {
		return nil, fmt.Errorf("claude: bad schema: %w", err)
	}
```

## Single durable-state writer, concurrent workers

`plan/run.go:80-100`

```go
		return nil, err
	}
	if err := r.preflight(p, true); err != nil {
		return nil, err
	}

	// EVERYTHING that mutates run state, and everything that WRITES it, happens in
	// this goroutine. Workers only make the expensive call and hand the result
	// back, so `Jobs` buys concurrency without touching the invariant that the
	// interpreter is the sole writer of durable state.
	done := map[string]StepResult{}
	if r.Root != nil {
		// The reserved root step: a value in the graph, not a special case in bind.
		done[RootStep] = StepResult{Produced: map[string]dawn.Ref{"workspace": *r.Root}}
	}

	sch := newSchedule(p, order)
	type finished struct {
		id  string
		res dawn.Result
		err error
```

## Content before pointer

`plan/run.go:285-320`

```go
		return StepResult{}, false, fmt.Errorf("step %q: %w", id, err)
	}
	rec.Ref = ref
	return rec, true, nil
}

// commit content-addresses a result and then records the pointer. Blob FIRST: a
// crash between the two leaves an orphan blob, which is harmless garbage, where
// the other order leaves a journal pointer to bytes that do not exist.
func (r *Runner) commit(id, key string, agent Agent, res dawn.Result) (StepResult, error) {
	blob, err := json.Marshal(stepBlob{Output: res.Output, Produced: res.Produced})
	if err != nil {
		return StepResult{}, err
	}
	ref, err := r.Blobs.Put(blob)
	if err != nil {
		return StepResult{}, err
	}
	if r.Journal != nil {
		if err := r.Journal.Append(Entry{
			Key: key, Ref: ref, Step: id, Agent: agent.String(),
			Tokens: &Tokens{In: res.Tokens.Input, Out: res.Tokens.Output,
				CacheRead: res.Tokens.CacheRead, CacheCreate: res.Tokens.CacheCreate},
		}); err != nil {
			return StepResult{}, err
		}
	}
	return StepResult{Output: res.Output, Produced: res.Produced, Tokens: res.Tokens, Ref: ref}, nil
}

// bound is a step's resolved inputs: what the agent is asked, what refs it
// receives, and the canonical form those inputs take in the identity key.
type bound struct {
	prompt string
	refs   map[string]dawn.Ref
	key    map[string]string
```

## Stable system prompt for cache hits

`backend/claude/claude.go:70-95`

```go
// the raw assistant text under the "text" key.
func (b Backend) Invoke(ctx context.Context, in dawn.Invocation) (dawn.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, timeoutOr(b.Timeout))
	defer cancel()
	model := in.Model
	if model == "" {
		model = b.Model
	}
	// A STABLE system prompt is the whole caching story for this backend. Claude
	// Code's default preset embeds per-machine sections (cwd, env, git status)
	// that drift a few tokens between runs; prefix matching is exact, so every
	// byte after the drift point recomputes and the caller's content never caches.
	// Measured: with the default preset, cache_creation stays ~20.5k on EVERY call
	// and cache_read never covers the caller's prompt. Passing an explicit system
	// prompt replaces the preset with bytes dawn controls, and the same content then
	// reads from cache across unrelated invocations.
	//
	// Replacing the preset is right HERE and wrong for Workspace: this backend
	// makes one prompt-to-JSON call and needs no file tools, while an editing agent
	// does. See workspace.go for the other half.
	system := in.System
	if system == "" {
		system = defaultSystem
	}
	schemaFlags, err := schemaArgs(in.Schema)
	if err != nil {
```


## A platform is a decision, not a fallback

`platform.go` (deleted; the constraint returns when v0 has a primitive to constrain)

```go
//go:build !darwin && !linux

package dawn

// dawn supports macOS and Linux. Building anywhere else stops here, on purpose,
// with the identifier below as the message.
//
// The alternative was what shipped before: build everywhere, and hand the
// unsupported platforms a no-op. On Windows that meant `lockFile` returned nil,
// so "one run per state directory" was not enforced and two runs both paid; and
// `killGroup` killed only the direct child, so a timed-out agent left its tool
// subprocesses holding the inherited pipe — the exact hang the proc package
// exists to prevent. Both guarantees are documented. Neither held. Nothing said
// so, because a binary that builds looks like a binary that works.
//
// Adding a platform is therefore a DECISION, not a fallback: implement the two
// primitives there (on Windows, LockFileEx and a Job Object, which need
// golang.org/x/sys/windows and would be dawn's second dependency), prove them,
// and delete a term from the constraint above. Until someone does that, WSL is
// Linux and works today.
//
// The refusal lives in the root package because every other package imports it,
// so one file covers the whole module.
var _ = dawn_supports_macOS_and_Linux_only__see_platform_go
```

Note for v0: the netns-only-route-out guarantee is Linux-native; on macOS Docker runs in a VM.
Whether the boundary holds identically on both is an open question the design does not settle.
