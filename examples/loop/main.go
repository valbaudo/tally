// Command loop is the agent loop, written out.
//
// dawn's other example, examples/hunt, runs agents by exec'ing a vendor CLI and
// reading the file it leaves behind. That is an honest shape and it is not this
// one: there the turn loop lives inside a binary the harness cannot see into,
// so "control flow is a for loop in your main.go" is a loop over whole process
// invocations. Here the loop is ours. Every request is built in this process,
// every tool is a Go function, and every turn is a span in the ledger.
//
// # What an agent is
//
// A chat call is one request and one response. An agent is that same call in a
// for loop, plus one thing: the model's own output decides whether the loop
// runs again.
//
//	resp := client.Messages.New(ctx, params)     // ask
//	if resp.StopReason != "tool_use" { break }   // THE MODEL chose to stop
//	results := run(resp's tool_use blocks)       // WE choose what it learns
//	params.Messages = append(params.Messages,    // which is the next input
//	    resp.ToParam(), anthropic.NewUserMessage(results...))
//
// That is the entire mechanism. There is no state on the server, no session, no
// hidden scheduler: the conversation is a slice in this process and every turn
// re-sends all of it. "Stateless compute engine" is not a metaphor, it is the
// wire protocol.
//
// It is worth being exact about which half of the branch belongs to whom.
//
//   - The model decides WHICH tool and WITH WHAT. That is the conditional the
//     owner asked about — "do stuff according to other stuff that happens" —
//     and it is not written in Go anywhere. It is a stop_reason and a name.
//   - The harness decides WHAT COMES BACK. That is the guard, and it IS written
//     in Go. A tool_result carrying is_error:true is how "no, and here is why"
//     enters the conversation, as data on the next turn rather than as a
//     sterner sentence in the system prompt.
//
// So the loop below has exactly four in-run conditionals, and all four are the
// harness's:
//
//  1. the turn cap        — the CLIs have no --max-turns; this is where it lives
//  2. ctx                 — cancellation kills the run between turns
//  3. the tool result     — what the model gets to know
//  4. is_error            — whether that result reads as a fact or a rejection
//
// # Why file_finding is a tool and not a paragraph
//
// The agent cannot write a finding into prose we then parse, because there is
// no path from prose to the ledger here. The only way out of this loop is
// file_finding, whose input_schema is a Go struct and whose acceptance is
// cite() — which opens the file and counts its lines. A model can be argued out
// of an instruction. It cannot be argued out of os.ReadFile.
//
// Cloudflare's harness runs that check as a separate downstream stage. Running
// it inside the tool call instead buys one thing a downstream stage cannot: the
// agent is still alive and still holds its context, so a rejection is a
// correction it can act on rather than a report nobody reads.
//
// # Why every turn is its own span
//
// dawn puts the span id in the base URL, so the proxy attributes spend by the
// path a request arrived on. Point turn N at its own span's base URL and the
// ledger prices that turn on its own row — in, out, cache, usd — with no
// cooperation from this file. The harness never reports a number; it only
// chooses which span the request travels under.
//
// That distinction is the whole reason to do it this way. resp.Usage is the
// model telling us what it used, and a harness that writes resp.Usage into the
// ledger with Span.Fact is a harness marking its own homework. The span-per-turn
// costs two rows and buys a per-turn cost that came from the metering path.
//
// # Running it
//
//	go run ./examples/loop -src . -class "path traversal"
//
// ANTHROPIC_API_KEY must be set. It stays in this process: the loop below
// authenticates with the literal string "dawn", and the proxy attaches the real
// credential on the way out.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	glue "github.com/valbaudo/dawn"
)

// Bounds. maxOut is the output cap per turn; a turn that hits it comes back
// stop_reason=max_tokens with a half-written sentence, which is a Failed run
// and not something to retry blind. maxRead is how much of a file one
// read_file call may return, which is the only context discipline this example
// has and the only one it needs.
const (
	maxOut  = 8192
	maxRead = 400
)

// ------------------------------------------------------------------ artifact

// finding is the only artifact this agent can produce, and it is a Go struct
// before it is anything else: the tool's input_schema below is this struct's
// shape, and cite() is this struct's validity. There is exactly one way for a
// model to report something, and it has four required fields.
type finding struct {
	File   string `json:"file"`
	Lo     int    `json:"lo"`
	Hi     int    `json:"hi"`
	Threat string `json:"threat"`
	Claim  string `json:"claim"`
}

// ---------------------------------------------------------------- the tools

// A tool is a name, a description, and a JSON Schema. The description is not
// documentation — it is the only thing that tells the model WHEN to call this
// rather than what it does, and "call it only after read_file has shown you the
// lines" is a sentence that measurably changes behaviour.
var readTool = anthropic.ToolParam{
	Name: "read_file",
	Description: anthropic.String(
		"Read a file from the source tree. Lines come back numbered so you can cite them; " +
			"you may not cite a line you have not read. At most " + strconv.Itoa(maxRead) +
			" lines are returned per call, so pass `from` to continue past the cut. " +
			"The tree is read-only and there is no network: this and file_finding are all you have."),
	InputSchema: anthropic.ToolInputSchemaParam{
		Properties: map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "Repo-relative path, exactly as it appears in the listing you were given.",
			},
			"from": map[string]any{
				"type":        "integer",
				"description": "First line to return, 1-based. Omit for 1.",
			},
		},
		Required: []string{"path"},
	},
}

var fileTool = anthropic.ToolParam{
	Name: "file_finding",
	Description: anthropic.String(
		"File one vulnerability. Call this ONLY after read_file has shown you the exact lines " +
			"you are citing: the harness checks every citation against the file on disk before " +
			"it accepts anything, so a line number you guessed comes straight back as an error " +
			"and costs you a turn. If the code is safe, do not call this at all — say so and stop. " +
			"Zero findings is a correct outcome and it is cheaper than one that gets refuted."),
	InputSchema: anthropic.ToolInputSchemaParam{
		Properties: map[string]any{
			"file": map[string]any{
				"type":        "string",
				"description": "Repo-relative path holding the defect.",
			},
			"lo": map[string]any{
				"type":        "integer",
				"description": "First line of the defect, 1-based inclusive. The lines that are wrong — not the function, not the imports.",
			},
			"hi": map[string]any{
				"type":        "integer",
				"description": "Last line of the defect, 1-based inclusive. Must be a line that exists in the file.",
			},
			"threat": map[string]any{
				"type":        "string",
				"description": "Who the attacker is, what input they control that reaches these lines, and what they gain. Three sentences. The attack class name is not a threat model.",
			},
			"claim": map[string]any{
				"type":        "string",
				"description": "What is wrong with the cited lines, in one or two sentences.",
			},
		},
		Required: []string{"file", "lo", "hi", "threat", "claim"},
	},
}

// system is the role regime, and it is the same shape in every stage of every
// harness in this space: name the one job, name the order of work, name the
// stop condition, and make the cheap wrong answer expensive. Nothing here
// restates a rule that cite() enforces — a rule stated in both places is a rule
// with two copies, and the prose copy is the one that drifts.
const system = `You hunt exactly one attack class: %s. You have no other job.
Nothing outside that class is yours to report, however interesting it is.

You are one of many hunters. Other agents cover the other classes, so work
outside your scope is duplicated at best and noise at worst.

Work in this order and do not skip a step:

1. From the listing you were given, pick the files where this class could
   possibly live. Ignore the rest; you are not reading this tree, you are
   reading a handful of files in it.
2. read_file each candidate. You may not cite a line you have not read.
3. Before you file, state the threat model to yourself: who the attacker is,
   what input they control, what boundary is crossed, what they gain. If you
   cannot name all four, there is no finding. Say so and stop.
4. Then name the assumption that would make this a false positive — a caller
   that sanitizes, a length bounded upstream, a lock held on every path — and
   go check it with read_file. A different model whose only job is to destroy
   your finding reads it next, and it will find the caller you did not.
5. file_finding, then keep going only if you have another real one.

When you are done, say what you concluded and stop. A hunter that files nothing
and explains why is a successful hunter. A rejected finding costs more than a
missing one.`

// ------------------------------------------------------------- the post-check

// cite is the deterministic gate, and it is what "the harness can say no"
// means concretely. Nothing in it is negotiable by argument: os.ReadFile does
// not have an opinion about whether the file ought to exist.
//
// The error strings are written for the model, not for a log. "src/a.go has 42
// lines; you cited 1-99" is a correction it can act on this turn; "invalid
// citation" is a turn spent guessing.
func cite(root string, f finding) error {
	// filepath.Join cleans, so a path with .. in it resolves to where it
	// actually points and the prefix check sees it leave. One mechanism, not a
	// sanitizer plus a check that disagree at the third edit.
	p := filepath.Join(root, f.File)
	if !strings.HasPrefix(p, root+string(os.PathSeparator)) {
		return fmt.Errorf("%q is outside the source tree; cite a path from the listing", f.File)
	}
	src, err := os.ReadFile(p)
	if err != nil {
		return fmt.Errorf("%s does not exist; cite a path from the listing", f.File)
	}
	if n := len(lines(src)); f.Lo < 1 || f.Hi < f.Lo || f.Hi > n {
		return fmt.Errorf("%s has %d lines; you cited %d-%d", f.File, n, f.Lo, f.Hi)
	}
	// ponytail: a word count, not an entailment model. It exists because the
	// measured failure is a model filling `threat` with the class name it was
	// handed, which satisfies "required" and says nothing. Cost: a genuinely
	// terse threat model is rejected. Upgrade path: the jury already reads this
	// field, so make "the threat model is a slogan" a REFUTED verdict and
	// delete this.
	if w := len(strings.Fields(f.Threat)); w < 8 {
		return fmt.Errorf("threat model is %d words: name the attacker, the input they control, and what they gain", w)
	}
	return nil
}

// ------------------------------------------------------------------ the tools

// hunter is one agent: a source tree it may read, a span it spends under, and
// whatever it has managed to get past cite().
type hunter struct {
	root  string
	sw    *glue.Sweep
	span  *glue.Span
	filed []finding
}

// dispatch runs one tool call and returns what the model reads next turn. The
// bool is is_error, and it is this harness's entire vocabulary for "no".
func (h *hunter) dispatch(name, input string) (string, bool) {
	switch name {
	case readTool.Name:
		var in struct {
			Path string `json:"path"`
			From int    `json:"from"`
		}
		if err := json.Unmarshal([]byte(input), &in); err != nil {
			return "malformed input: " + err.Error(), true
		}
		return h.read(in.Path, in.From)

	case fileTool.Name:
		var f finding
		if err := json.Unmarshal([]byte(input), &f); err != nil {
			return "malformed input: " + err.Error(), true
		}
		if err := cite(h.root, f); err != nil {
			return "rejected: " + err.Error(), true
		}
		h.filed = append(h.filed, f)
		// One fact row per finding, value verbatim. This is the harness
		// writing down what it knows and dawn does not; the COST of the turn
		// that produced it came from the proxy, not from here.
		b, _ := json.Marshal(f)
		h.span.Fact("finding", b)
		return fmt.Sprintf("accepted: %s:%d-%d recorded as finding %d",
			f.File, f.Lo, f.Hi, len(h.filed)), false
	}
	// The model cannot call a tool that is not in Tools, so this is
	// unreachable. It is here because "unreachable" and "cannot happen" are
	// different words, and one of them is about a future edit to Tools.
	return "no such tool: " + name, true
}

// read returns numbered lines, bounded. The numbering is not cosmetic: it is
// the only way the model can produce a citation cite() will accept.
func (h *hunter) read(path string, from int) (string, bool) {
	p := filepath.Join(h.root, path)
	if !strings.HasPrefix(p, h.root+string(os.PathSeparator)) {
		return fmt.Sprintf("%q is outside the source tree", path), true
	}
	src, err := os.ReadFile(p)
	if err != nil {
		return fmt.Sprintf("%s does not exist; the listing is the whole tree", path), true
	}
	ls := lines(src)
	if from < 1 {
		from = 1
	}
	if from > len(ls) {
		return fmt.Sprintf("%s has %d lines; you asked to start at %d", path, len(ls), from), true
	}
	end := min(from+maxRead, len(ls)+1)
	var b strings.Builder
	for i := from; i < end; i++ {
		fmt.Fprintf(&b, "%6d  %s\n", i, ls[i-1])
	}
	if end <= len(ls) {
		fmt.Fprintf(&b, "\n[%d more lines; call read_file again with from=%d]\n", len(ls)-end+1, end)
	}
	return b.String(), false
}

// ------------------------------------------------------------------- the loop

// hunt is the loop. Everything above it is furniture.
//
// It returns dawn's five-way class and the free text that goes beside it, and
// it deliberately does not close h.span: the harness's own gate writes the
// class, and a function that both decides and records is a function you cannot
// test the decision of.
func (h *hunter) hunt(ctx context.Context, client anthropic.Client, model, class string, maxTurns int) (glue.Class, string) {
	adaptive := anthropic.ThinkingConfigAdaptiveParam{}
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(model),
		MaxTokens: maxOut,
		Thinking:  anthropic.ThinkingConfigParamUnion{OfAdaptive: &adaptive},
		System: []anthropic.TextBlockParam{{
			Text: fmt.Sprintf(system, class),
		}},
		Tools: []anthropic.ToolUnionParam{{OfTool: &readTool}, {OfTool: &fileTool}},
		// The first user message is state the harness computed, not a
		// question. Handing over the file list costs a few hundred tokens once
		// and saves a directory-listing tool plus the turns spent using it.
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(
				"The source tree is read-only and contains these files:\n\n" +
					listing(h.root) + "\nBegin.")),
		},
	}

	for turn := 1; ; turn++ {
		// Conditional 1. The CLIs have no --max-turns; in this shape the cap
		// is a for loop, which is the entire difference.
		if turn > maxTurns {
			return glue.Failed, fmt.Sprintf("turn cap: %d turns without end_turn, %d filed", maxTurns, len(h.filed))
		}

		// One span per turn, and the request travels under its base URL. The
		// proxy reads the span id out of the path, so this turn's tokens and
		// dollars land on this turn's row whatever this process later claims.
		ts := h.sw.Span(h.span, fmt.Sprintf("turn %d", turn), nil)

		// Conditional 2 is ctx: cancel it and the run ends here, between
		// turns, with the ledger consistent.
		resp, err := client.Messages.New(ctx, params, option.WithBaseURL(ts.BaseURL("anthropic")))
		if err != nil {
			c, why := refused(err)
			ts.Close(c, why)
			return c, fmt.Sprintf("turn %d: %s", turn, why)
		}
		ts.Close(glue.OK, string(resp.StopReason))

		// The assistant turn goes into history BEFORE its tool calls run. If
		// it did not, the tool_result blocks below would answer tool_use ids
		// the conversation never contained, and the next request is a 400.
		params.Messages = append(params.Messages, resp.ToParam())

		if resp.StopReason != anthropic.StopReasonToolUse {
			return h.stopped(resp.StopReason, turn)
		}

		var results []anthropic.ContentBlockParamUnion
		for _, block := range resp.Content {
			// ContentBlockUnion is one flattened struct; AsAny gives the real
			// variant. Thinking and text blocks are in here too and are not
			// ours to answer.
			use, ok := block.AsAny().(anthropic.ToolUseBlock)
			if !ok {
				continue
			}
			// Input is json.RawMessage, so the raw bytes are what to unmarshal.
			// Conditionals 3 and 4 are both inside dispatch: what comes back,
			// and whether it comes back as a rejection.
			out, isErr := h.dispatch(use.Name, use.JSON.Input.Raw())
			results = append(results, anthropic.NewToolResultBlock(use.ID, out, isErr))
		}
		if len(results) == 0 {
			// stop_reason said tool_use and no tool_use block arrived. An
			// empty user message is a 400, so ending here beats looping on a
			// request the API will reject.
			return glue.Failed, fmt.Sprintf("turn %d: stop_reason tool_use with no tool_use block", turn)
		}
		// Every result in ONE user message, however many tools ran.
		params.Messages = append(params.Messages, anthropic.NewUserMessage(results...))
	}
}

// stopped maps the model's reason for stopping onto dawn's five-way class. The
// distinction that earns its keep is Rejected versus Failed: Rejected means
// re-running this unchanged spends twice for the same answer, Failed means the
// attempt broke and a changed attempt is worth making.
func (h *hunter) stopped(r anthropic.StopReason, turn int) (glue.Class, string) {
	switch r {
	case anthropic.StopReasonEndTurn:
		return glue.OK, fmt.Sprintf("%d filed in %d turns", len(h.filed), turn)

	case anthropic.StopReasonRefusal:
		// The model declined the task. Nothing about the request will be
		// different next time, so this is Rejected and not Failed.
		return glue.Rejected, fmt.Sprintf("refusal on turn %d, %d filed", turn, len(h.filed))

	case anthropic.StopReasonMaxTokens:
		// The turn was cut mid-sentence, so the assistant message now in
		// history is a fragment and any tool_use inside it may be half-written
		// JSON carrying an id we would be answering blind. Raising MaxTokens is
		// the fix; another request on this history is not.
		return glue.Failed, fmt.Sprintf("max_tokens on turn %d: MaxTokens is %d, raise it", turn, maxOut)

	case anthropic.StopReasonModelContextWindowExceeded:
		// The externalized-state failure, named. The conversation outgrew the
		// window because everything read this run is still in it. The fix is a
		// smaller maxRead or a working set on disk, not a bigger model.
		return glue.Failed, fmt.Sprintf("context window exhausted on turn %d, %d filed", turn, len(h.filed))
	}
	// stop_sequence and pause_turn, neither of which this configuration can
	// produce: no StopSequences are set and no server-side tool is declared.
	return glue.Failed, fmt.Sprintf("stop_reason %q on turn %d", r, turn)
}

// refused classifies a failed request. dawn's proxy answers "you may not spend"
// with 429 and x-should-retry:false, which the SDK honours, so a 429 that
// reaches this function has already outlived every retry the SDK would make —
// budget or upstream limit, both mean do not re-run this unchanged.
func refused(err error) (glue.Class, string) {
	var api *anthropic.Error
	if errors.As(err, &api) && api.StatusCode == 429 {
		return glue.Rejected, "429: " + strings.TrimSpace(api.Error())
	}
	return glue.Failed, err.Error()
}

// ------------------------------------------------------------------- helpers

// lines splits without inventing a trailing empty one, so len(lines(b)) is the
// number a human counts and the number cite() compares against.
func lines(b []byte) []string {
	s := strings.TrimSuffix(string(b), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// listing is the bounded projection this example ships: the tree, with line
// counts, capped. It is what the agent gets instead of a directory tool.
func listing(root string) string {
	var out []string
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return nil
		case d.IsDir():
			if strings.HasPrefix(d.Name(), ".") && p != root {
				return fs.SkipDir
			}
			return nil
		case !d.Type().IsRegular() || strings.HasPrefix(d.Name(), "."):
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		b, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		out = append(out, fmt.Sprintf("  %s (%d lines)", rel, len(lines(b))))
		return nil
	})
	sort.Strings(out)
	if len(out) > 400 {
		out = append(out[:400], fmt.Sprintf("  [%d more files omitted]", len(out)-400))
	}
	return strings.Join(out, "\n") + "\n"
}

// ---------------------------------------------------------------------- main

func main() {
	src := flag.String("src", ".", "source tree to hunt in; never written to")
	class := flag.String("class", "path traversal", "the one attack class this hunter may report")
	model := flag.String("model", "claude-sonnet-5", "model id")
	turns := flag.Int("turns", 24, "hard cap on turns")
	budget := flag.Float64("budget", 2, "usd this sweep may spend, total")
	flag.Parse()

	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		log.Fatal("ANTHROPIC_API_KEY is unset. It stays in this process: the loop " +
			"authenticates as \"dawn\" and the proxy attaches the real credential.")
	}
	root, err := filepath.Abs(*src)
	if err != nil {
		log.Fatal(err)
	}

	// Isolate:false — this harness launches no containers, so there is no
	// bridge to build. Every call still goes through the proxy and is still
	// metered, priced and ledgered, because a predicate the harness runs in its
	// own process takes exactly the path an agent's does.
	sw, err := glue.Open(glue.Config{
		Sweep:  "loop",
		DB:     "loop.db",
		Keys:   map[string]string{"anthropic": key},
		Budget: glue.USD(*budget),
	})
	if err != nil {
		log.Fatal(err)
	}
	defer sw.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	go sw.Serve(ctx)

	span := sw.Span(nil, "hunt "+*class, sw.Budget())
	h := &hunter{root: root, sw: sw, span: span}

	// The API key this client holds is the string "dawn". The proxy discards
	// it and attaches the real one; a client with no credential is the whole
	// point of pointing it at a span.
	c, why := h.hunt(ctx, anthropic.NewClient(option.WithAPIKey("dawn")), *model, *class, *turns)
	span.Close(c, why)
	fmt.Printf("\n%s: %s\n\n", c, why)

	report(sw)
	if err := sw.Err(); err != nil {
		log.Fatal(err)
	}
}

// report prints what the ledger says, which is the point of the span-per-turn.
// Every number below was written by the proxy on the way through; nothing in
// this file reported a token or a dollar.
func report(sw *glue.Sweep) {
	outs, err := sw.Outcomes()
	if err != nil {
		log.Fatal(err)
	}
	sort.Slice(outs, func(i, j int) bool { return outs[i].Opened.Before(outs[j].Opened) })
	fmt.Printf("%-22s %-9s %6s %8s %8s %10s  %s\n", "span", "class", "calls", "in", "out", "usd", "detail")
	for _, o := range outs {
		fmt.Printf("%-22s %-9s %6d %8d %8d %10.5f  %s\n",
			o.Name, o.Class, o.Calls, o.In, o.Out, float64(o.Spend), o.Detail)
		for _, f := range o.Facts {
			fmt.Printf("    %s %s\n", f.Key, f.Value)
		}
	}
}
