package dawn

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// writeReceiptJSON drops one receipt.json by hand, in the shape writeReceipt
// itself would have produced, under attempts/<stage>/<attempt>/ — report is
// tested against the file it reads, not against writeReceipt's own output.
func writeReceiptJSON(t *testing.T, dir, stage string, attempt int, body string) {
	t.Helper()
	d := filepath.Join(dir, "attempts", stage, strconv.Itoa(attempt))
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "receipt.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestReportRendersEachGateKind covers all three gate kinds, plus a nil Drew,
// in one run — the whole point of a receipt is that all four combinations
// read back deterministically and never invent a claim (0 tokens, a % of
// something, a cross-agent total) the evidence does not support.
func TestReportRendersEachGateKind(t *testing.T) {
	dir := t.TempDir()
	const distinctiveReason = "no oracle exists for this stage's own claim"

	writeReceiptJSON(t, dir, "build", 1, `{
		"stage": "build", "attempt": 1, "id": "id1", "agent": "claude-code",
		"gate": "sound", "image": "g@sha256:aaa", "state": "passed",
		"metrics": {"reward": 1},
		"drew": {"input_tokens": 168431, "cache_tokens": 158059, "output_tokens": 625, "cost_usd": 0.0793}
	}`)
	writeReceiptJSON(t, dir, "build", 2, `{
		"stage": "build", "attempt": 2, "id": "id4", "agent": "claude-code",
		"gate": "sound", "image": "g@sha256:aaa", "state": "infra_error",
		"drew": null
	}`)
	writeReceiptJSON(t, dir, "format", 1, `{
		"stage": "format", "attempt": 1, "id": "id2", "agent": "codex",
		"gate": "format_only", "image": "g@sha256:bbb", "state": "unverified",
		"metrics": {"reward": 1},
		"drew": null
	}`)
	writeReceiptJSON(t, dir, "nogate", 1, `{
		"stage": "nogate", "attempt": 1, "id": "id3", "agent": "claude-code",
		"gate": "none", "reason": "`+distinctiveReason+`", "state": "unverified",
		"drew": null
	}`)
	if err := os.WriteFile(filepath.Join(dir, "record.json"), []byte(`{"state":"passed","fix_reward":1}`), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := report(dir)
	if err != nil {
		t.Fatalf("report: %v", err)
	}

	for _, want := range []string{
		"well-formed, not correct",
		distinctiveReason,
		"sound gate",
		"no gate",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report does not contain %q:\n%s", want, got)
		}
	}
	// Four adjacent "unknown" cells is what a nil Drew's row renders — not
	// just the word "unknown" in isolation, which the fixed "not known"
	// footer also contains and would make the assertion pass even if a nil
	// Drew wrongly rendered 0.
	if !strings.Contains(got, "unknown | unknown | unknown | unknown") {
		t.Errorf("report does not render a nil Drew as unknown in all four numeric columns:\n%s", got)
	}
	for _, unwanted := range []string{"%", "total"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("report contains %q, which no figure here is entitled to claim:\n%s", unwanted, got)
		}
	}
}

// A live gate's receipt must render its hosts in the heading, framed as
// reachable rather than reached, and the report's footer must carry the
// caveat that its pass is a claim about a moment, not about pinned bytes.
//
// This goes through the real writeReceipt, not a hand-crafted receipt.json:
// receipt.Gate comes from Gate.kind(), so this is also the test that ties a
// regression in kind() (the "live" case falling through to "sound") to a
// visible failure here, not just in the kind() test itself.
func TestReportRendersLiveGateHostsAndCaveat(t *testing.T) {
	dir := t.TempDir()
	s := Stage{ID: "exploit", Agent: ClaudeCode, Gate: LiveGate(Image("g@sha256:aaa"), "target.example.com", "10.0.0.5")}
	evidence := filepath.Join(dir, "attempts", s.ID, "1")
	if err := os.MkdirAll(evidence, 0o755); err != nil {
		t.Fatal(err)
	}
	writeReceipt(evidence, s, 1, Result{State: Passed, metrics: map[string]float64{"reward": 1}})

	got, err := report(dir)
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	for _, want := range []string{
		"live gate",
		"reachable, not reached",
		"dawn observes no traffic",
		"target.example.com, 10.0.0.5",
		"was exploitable at that moment",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report does not contain %q:\n%s", want, got)
		}
	}
}

// A receipt carrying a model renders it; one carrying none renders
// "unpinned" — a claim dawn is entitled to make ("I set nothing"), not
// drawCell's "unknown" ("nothing was reported to me").
func TestReportRendersModelOrUnpinned(t *testing.T) {
	dir := t.TempDir()
	withModel := Stage{ID: "a", Agent: ClaudeCode, Gate: NoGate("test")}
	withoutModel := Stage{ID: "b", Agent: Codex, Gate: NoGate("test")}
	for _, s := range []Stage{withModel, withoutModel} {
		evidence := filepath.Join(dir, "attempts", s.ID, "1")
		if err := os.MkdirAll(evidence, 0o755); err != nil {
			t.Fatal(err)
		}
		writeReceipt(evidence, s, 1, Result{State: Unverified})
	}

	got, err := report(dir)
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if !strings.Contains(got, "claude-sonnet-5") {
		t.Errorf("report does not render the pinned model:\n%s", got)
	}
	if !strings.Contains(got, "unpinned") {
		t.Errorf("report does not render an unset model as unpinned:\n%s", got)
	}
}

// Metrics render sorted by name, so the same evidence always renders the same
// bytes regardless of map iteration order.
func TestReportMetricsAreSortedAndDeterministic(t *testing.T) {
	dir := t.TempDir()
	writeReceiptJSON(t, dir, "s", 1, `{
		"stage": "s", "attempt": 1, "id": "id1", "agent": "claude-code",
		"gate": "none", "reason": "x", "state": "unverified",
		"metrics": {"z": 2, "a": 1},
		"drew": null
	}`)
	got, err := report(dir)
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if !strings.Contains(got, "a=1, z=2") {
		t.Errorf("report does not render metrics sorted by name:\n%s", got)
	}
}

// A receipt that fails to parse is an error, never a silently skipped row —
// dropping one attempt is exactly the "ledger disagrees with the evidence"
// bug this file exists to make impossible.
func TestReportCorruptReceiptIsAnError(t *testing.T) {
	dir := t.TempDir()
	writeReceiptJSON(t, dir, "s", 1, "not json")
	if _, err := report(dir); err == nil {
		t.Fatal("report returned nil error for a corrupt receipt.json")
	}
}
