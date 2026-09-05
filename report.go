package dawn

// The run's receipt. report is a pure function of the run directory — every
// number it prints is read back off the same receipt.json and record.json a
// human would open by hand — never a second in-memory ledger that has to
// agree with the evidence by convention. There is no root report.json,
// because the per-attempt receipts already are it: this file only renders
// them.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// receipt is one dispatched attempt, written once by writeReceipt and read
// back, many at a time, by report. Its ID is attemptID(s, attempt) — the same
// hash that keys actuations.json — so a receipt can always be joined back to
// the effect it did or did not authorize.
type receipt struct {
	Stage   string             `json:"stage"`
	Attempt int                `json:"attempt"`
	ID      string             `json:"id"` // attemptID: joins actuations.json
	Agent   string             `json:"agent"`
	Gate    string             `json:"gate"`             // sound | format_only | none
	Image   Image              `json:"image,omitempty"`  // the gate's image; absent when there is none
	Reason  string             `json:"reason,omitempty"` // NoGate's mandatory reason, verbatim
	State   State              `json:"state"`
	Metrics map[string]float64 `json:"metrics,omitempty"` // everything the gate wrote
	Drew    *draw              `json:"drew"`              // null: Harbor wrote no agent_result
}

// writeReceipt writes one attempt's receipt.json into its evidence
// directory. dispatchAttempt calls this on BOTH the live and the resumed
// path, with the same bytes either way — there is no `if !resumed` here,
// because a resumed Result is classify() over the same trial and so
// reconstructs identically. ID is computed here, from s and attempt, rather
// than read off r: a retried infra_error's Result has no attemptID yet
// (Scope.Run only sets that on the terminal Result), and this has to write a
// receipt for that attempt too.
//
// On any failure this calls bug() — the same reasoning as the evidence
// directory itself: a receipt dawn cannot write is an attempt with no
// record.
func writeReceipt(evidence string, s Stage, attempt int, r Result) {
	rec := receipt{
		Stage:   s.ID,
		Attempt: attempt,
		ID:      attemptID(s, attempt),
		Agent:   s.Agent.name,
		Gate:    s.Gate.kind(),
		Image:   s.Gate.image,
		Reason:  s.Gate.reason,
		State:   r.State,
		Metrics: r.metrics,
		Drew:    r.drew,
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		bug("cannot marshal receipt for %s attempt %d: %v", s.ID, attempt, err)
	}
	if err := os.WriteFile(filepath.Join(evidence, "receipt.json"), b, 0o644); err != nil {
		bug("cannot write receipt for %s attempt %d: %v", s.ID, attempt, err)
	}
}

// report renders dir's run as Markdown. A pure function of what is already on
// disk: glob every receipt.json, sort it, read back record.json if there is
// one, render. A receipt that fails to parse is an error, never a skipped
// row — silently dropping one attempt is exactly the kind of "believe the
// ledger, not the evidence" bug this file exists to avoid.
func report(dir string) (string, error) {
	hits, err := filepath.Glob(filepath.Join(dir, "attempts", "*", "*", "receipt.json"))
	if err != nil {
		return "", err
	}
	receipts := make([]receipt, 0, len(hits))
	for _, hit := range hits {
		b, err := os.ReadFile(hit)
		if err != nil {
			return "", fmt.Errorf("dawn: report: %s: %w", hit, err)
		}
		var rec receipt
		if err := json.Unmarshal(b, &rec); err != nil {
			return "", fmt.Errorf("dawn: report: %s: %w", hit, err)
		}
		receipts = append(receipts, rec)
	}
	sort.Slice(receipts, func(i, j int) bool {
		if receipts[i].Stage != receipts[j].Stage {
			return receipts[i].Stage < receipts[j].Stage
		}
		return receipts[i].Attempt < receipts[j].Attempt
	})

	record := map[string]any{}
	b, err := os.ReadFile(filepath.Join(dir, "record.json"))
	switch {
	case err == nil:
		if err := json.Unmarshal(b, &record); err != nil {
			return "", fmt.Errorf("dawn: report: record.json: %w", err)
		}
	case os.IsNotExist(err):
		// Missing is fine — a run that unwound before Record ever fired
		// still gets a report, just an empty record section.
	default:
		return "", fmt.Errorf("dawn: report: record.json: %w", err)
	}

	return render(dir, receipts, record), nil
}

// writeReport writes dir's report to dir/report.md. Panics on failure — the
// same loudness as run.flush, and for the same reason: a run whose receipt
// cannot be produced is not a run anyone can trust just because the process
// exited 0.
func writeReport(dir string) {
	text, err := report(dir)
	if err != nil {
		panic(fmt.Sprintf("dawn: report: %v", err))
	}
	if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte(text), 0o644); err != nil {
		panic(fmt.Sprintf("dawn: report: %v", err))
	}
}

// render is report's pure formatting step, split out so report keeps the
// I/O and this keeps the layout.
func render(dir string, receipts []receipt, record map[string]any) string {
	state, ok := record["state"].(string)
	if !ok {
		state = "unknown"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# %s — %s\n\n", filepath.Base(dir), state)

	for i := 0; i < len(receipts); {
		j := i + 1
		for j < len(receipts) && receipts[j].Stage == receipts[i].Stage {
			j++
		}
		renderStage(&b, receipts[i:j])
		i = j
	}

	renderRecord(&b, record)
	b.WriteString(notKnown)
	return b.String()
}

// renderStage renders one stage's heading, sized to its gate's soundness, and
// the one table every stage gets regardless of that soundness.
func renderStage(b *strings.Builder, rs []receipt) {
	head := rs[0]
	switch head.Gate {
	case "sound":
		fmt.Fprintf(b, "## %s — sound gate\n`%s`\n\n", head.Stage, head.Image)
	case "format_only":
		fmt.Fprintf(b, "## %s — format-only gate: its metrics say well-formed, not correct\n`%s`\n\n", head.Stage, head.Image)
	default: // "none"
		fmt.Fprintf(b, "## %s — no gate\nreason: %s\n\n", head.Stage, head.Reason)
	}

	b.WriteString("| attempt | state | metrics | agent | input | cache | output | cost_usd (est.) |\n")
	b.WriteString("|--:|---|---|---|--:|--:|--:|--:|\n")
	for _, r := range rs {
		fmt.Fprintf(b, "| %d | %s | %s | %s | %s | %s | %s | %s |\n",
			r.Attempt, r.State, renderMetrics(r.Metrics), r.Agent,
			drawCell(r.Drew, func(d *draw) string { return strconv.Itoa(d.InputTokens) }),
			drawCell(r.Drew, func(d *draw) string { return strconv.Itoa(d.CacheTokens) }),
			drawCell(r.Drew, func(d *draw) string { return strconv.Itoa(d.OutputTokens) }),
			drawCell(r.Drew, func(d *draw) string { return strconv.FormatFloat(d.CostUSD, 'f', 4, 64) }),
		)
	}
	b.WriteString("\n")
}

// drawCell renders one numeric column of a nil Drew as "unknown" — never as
// 0, which would claim an attempt that reported nothing drew nothing.
func drawCell(d *draw, f func(*draw) string) string {
	if d == nil {
		return "unknown"
	}
	return f(d)
}

// renderMetrics formats a gate's written metrics deterministically: sorted by
// name, so the same run always renders the same bytes.
func renderMetrics(m map[string]float64) string {
	if len(m) == 0 {
		return "—"
	}
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, len(names))
	for i, name := range names {
		parts[i] = name + "=" + strconv.FormatFloat(m[name], 'g', -1, 64)
	}
	return strings.Join(parts, ", ")
}

// renderRecord renders every key Record ever wrote for this run, sorted, as
// compact JSON — the same bytes flush() persisted, not a re-summary of them.
// Omitted entirely when there is nothing to say, rather than an empty
// heading.
func renderRecord(b *strings.Builder, record map[string]any) {
	if len(record) == 0 {
		return
	}
	names := make([]string, 0, len(record))
	for name := range record {
		names = append(names, name)
	}
	sort.Strings(names)
	b.WriteString("## record\n\n")
	for _, name := range names {
		v, _ := json.Marshal(record[name])
		fmt.Fprintf(b, "%s: %s\n", name, v)
	}
	b.WriteString("\n")
}

// notKnown is printed verbatim at the end of every report: the caveats that
// are true of every run regardless of what it did, because they are facts
// about Harbor and the providers, not about this run's own arithmetic.
const notKnown = "## not known\n\n" +
	"- `cost_usd` is Harbor's estimate of list-price value, not an observed charge:\n" +
	"  dawn never sees the provider's bill. codex's figure is always a litellm\n" +
	"  estimate, and one unpriced call zeroes a whole trial's cost. Nothing above is\n" +
	"  summed across agents, and nothing above is a share of a budget — the figures\n" +
	"  are not comparable between agents, and there is no denominator.\n" +
	"- Any attempt above that did not pass may have hit a provider quota wall dawn\n" +
	"  cannot see. claude exits 0 on that failure, and Harbor's classifier matches\n" +
	"  the Console billing wording rather than the consumer limit message, so a\n" +
	"  silent wall and a genuine result are indistinguishable from here.\n" +
	"- An attempt whose draw reads `unknown` reported nothing to Harbor. What it\n" +
	"  drew is recorded nowhere, and is not zero.\n" +
	"- What a format-only or no-gate stage did NOT check is the author's to state,\n" +
	"  not dawn's to infer: see each stage's reason, and the record above.\n"
