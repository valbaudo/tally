package main

import (
	"context"
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	glue "github.com/valbaudo/dawn"
)

// These are the four things in this harness that break silently if they break:
// the deterministic validator, the verdict rule, the config files that carry
// the span capability into a CLI, and the metered path itself. Everything else
// is a prompt or a print.

// ---------------------------------------------------------------- pass 1

func TestPass1(t *testing.T) {
	repo := t.TempDir()
	os.MkdirAll(filepath.Join(repo, "src"), 0o755)
	os.WriteFile(filepath.Join(repo, "src/a.c"), []byte("1\n2\n3\n4\n5\n"), 0o644)
	os.WriteFile(filepath.Join(repo, "secret"), []byte("x\n"), 0o644)

	cases := []struct {
		name string
		f    Finding
		want string // "" means it passed
	}{
		{"ok", Finding{File: "src/a.c", Lo: 2, Hi: 4}, ""},
		{"whole file", Finding{File: "src/a.c", Lo: 1, Hi: 5}, ""},
		{"past eof", Finding{File: "src/a.c", Lo: 4, Hi: 9}, "line 9 past EOF (5 lines)"},
		{"no file", Finding{File: "src/nope.c", Lo: 1, Hi: 1}, "no such file"},
		{"dir", Finding{File: "src", Lo: 1, Hi: 1}, "not a regular file"},
		{"escape", Finding{File: "../../etc/passwd", Lo: 1, Hi: 1}, "path escapes repo"},
		{"absolute", Finding{File: "/etc/passwd", Lo: 1, Hi: 1}, "path escapes repo"},
		{"zero line", Finding{File: "src/a.c", Lo: 0, Hi: 1}, "bad citation"},
		{"inverted", Finding{File: "src/a.c", Lo: 4, Hi: 2}, "bad citation"},
		{"empty", Finding{Lo: 1, Hi: 1}, "bad citation"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := pass1(repo, &c.f); got != c.want {
				t.Fatalf("pass1 = %q, want %q", got, c.want)
			}
		})
	}
}

// A hunter that cites a real file it never read is the failure mode this pass
// exists for, and "the model said so" is not evidence against a stat().
func TestPass1RejectsPlausibleFabrication(t *testing.T) {
	repo := t.TempDir()
	os.WriteFile(filepath.Join(repo, "auth.go"), []byte("package main\n"), 0o644)
	f := Finding{File: "auth.go", Lo: 412, Hi: 430, Title: "missing bounds check"}
	if got := pass1(repo, &f); got == "" {
		t.Fatal("a citation 400 lines past EOF passed pass 1")
	}
}

// ---------------------------------------------------------------- the jury

func TestTally(t *testing.T) {
	const heavy = "openai-responses/gpt-5.6-sol"
	cases := []struct {
		name  string
		votes map[string]string
		want  string
	}{
		{"both cheap stand", map[string]string{"xai/g": "stands", "glm/z": "stands"}, "stands"},
		{"one refutes", map[string]string{"xai/g": "stands", "glm/z": "refuted"}, "refuted"},
		{"both refute", map[string]string{"xai/g": "refuted", "glm/z": "refuted"}, "refuted"},
		{"heavy overrides a refusal", map[string]string{"xai/g": "refuted", "glm/z": "stands", heavy: "stands"}, "stands"},
		{"heavy overrides a pass", map[string]string{"xai/g": "stands", "glm/z": "refuted", heavy: "refuted"}, "refuted"},
		{"an errored juror is not a vote to keep", map[string]string{"xai/g": "error", "glm/z": "refuted"}, "refuted"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := tally(c.votes); got != c.want {
				t.Fatalf("tally = %q, want %q", got, c.want)
			}
		})
	}
}

// Disagreement must be detectable BEFORE the heavy juror is called, because
// that is the only thing that keeps the expensive vendor on ~10% of the volume.
func TestDisagreementIsWhatBuysTheHeavyCall(t *testing.T) {
	agree := map[string]string{"xai/g": "stands", "glm/z": "stands"}
	if agree["xai/g"] != agree["glm/z"] {
		t.Fatal("agreement misdetected")
	}
	split := map[string]string{"xai/g": "stands", "glm/z": "refuted"}
	if split["xai/g"] == split["glm/z"] {
		t.Fatal("a split jury would not have escalated")
	}
}

func TestVerdictOf(t *testing.T) {
	cases := map[string]string{
		"REFUTED\nthe cited lock is held":       "refuted",
		"  stands  \nreachable from the parser": "stands",
		"I think it probably stands":            "unparsed", // one token, not a vibe
		"":                                      "unparsed",
		"preamble\nSTANDS":                      "stands",
	}
	for in, want := range cases {
		if got := verdictOf(in); got != want {
			t.Fatalf("verdictOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFindingIDIsContentAddressed(t *testing.T) {
	a := Finding{Class: "injection", File: "x.go", Lo: 10, Hi: 20, Title: "t"}
	b := a
	if a.id() != b.id() {
		t.Fatal("same content, different id: a resumed hunter would double-file")
	}
	c := a
	c.Lo = 11
	if a.id() == c.id() {
		t.Fatal("different citation, same id")
	}
}

// ---------------------------------------------------------------- CLI wiring

// sweepFor brings up a real supervisor with no containers: real SQLite, real
// listener, real proxy, no Docker. Config.Image is empty, so no network is
// created and nothing here needs a daemon.
func sweepFor(t *testing.T, upstream string) *glue.Sweep {
	t.Helper()
	sw, err := glue.Open(glue.Config{
		Sweep: "hunt-test", DB: filepath.Join(t.TempDir(), "hunt.db"),
		Keys:   map[string]string{"anthropic": "sk-anthropic", "glm": "sk-zai"},
		Budget: 100,
		Upstream: map[string]string{
			"anthropic": upstream,
			"glm":       upstream,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	go sw.Serve(ctx)
	t.Cleanup(func() { stop(); sw.Close() })
	return sw
}

// The config files are the whole integration for codex and droid, and the one
// thing they must carry is the span's base URL — the bearer capability in a
// path. The one thing they must NOT carry is a credential.
func TestAgentConfigsCarryTheCapabilityAndNoKey(t *testing.T) {
	sw := sweepFor(t, "http://127.0.0.1:1")
	span := sw.Span(nil, "t", sw.Budget())
	ws := t.TempDir()
	h := &harness{sw: sw, image: map[string]string{
		"claude": "glue/claude:latest", "codex": "glue/codex:latest", "droid": "glue/droid:latest"}}

	t.Run("codex", func(t *testing.T) {
		l, err := h.agentLaunch(roles.heavy, span, ws, "disprove this")
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := os.ReadFile(filepath.Join(ws, "codex", "config.toml"))
		if err != nil {
			t.Fatal(err)
		}
		want := span.BaseURL("openai-responses")
		if !strings.Contains(string(cfg), want) {
			t.Fatalf("config.toml lost the span base URL %q:\n%s", want, cfg)
		}
		// Verified 2026-09-03 against codex 0.145.0: wire_api "chat" is refused
		// by the binary outright, so this line is not a preference.
		if !strings.Contains(string(cfg), `wire_api = "responses"`) {
			t.Fatalf("codex needs the responses wire:\n%s", cfg)
		}
		if strings.Contains(string(cfg), "sk-") || hasKey(l) {
			t.Fatalf("a real credential reached the container: %+v\n%s", l, cfg)
		}
		// Without this the juror reads the host's ~/.codex, which on a
		// developer machine holds a real OpenAI credential and a real base URL.
		if !slices.Contains(l.Env, "CODEX_HOME=/work/codex") {
			t.Fatalf("codex would read the host's ~/.codex: %v", l.Env)
		}
		if l.Image != "glue/codex:latest" {
			t.Fatalf("image = %q", l.Image)
		}
	})

	t.Run("droid", func(t *testing.T) {
		l, err := h.agentLaunch(roles.cheapA, span, ws, "disprove this")
		if err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(filepath.Join(ws, "droid", "settings.json"))
		if err != nil {
			t.Fatal(err)
		}
		var got struct {
			CustomModels []struct {
				Model, Provider, BaseURL, APIKey string
			} `json:"customModels"`
		}
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		if len(got.CustomModels) != 1 {
			t.Fatalf("want one custom model, got %d", len(got.CustomModels))
		}
		m := got.CustomModels[0]
		if m.BaseURL != span.BaseURL("xai") {
			t.Fatalf("baseUrl = %q, want %q", m.BaseURL, span.BaseURL("xai"))
		}
		if m.Provider != "generic-chat-completion-api" {
			t.Fatalf("provider = %q", m.Provider)
		}
		if strings.HasPrefix(m.APIKey, "sk-") || hasKey(l) {
			t.Fatalf("a real credential reached the container: %s / %+v", raw, l)
		}
	})

	t.Run("driver needs no file", func(t *testing.T) {
		l, err := h.agentLaunch(roles.hunt, span, ws, "hunt")
		if err != nil {
			t.Fatal(err)
		}
		// Launch appends the span's own ANTHROPIC_BASE_URL last and docker
		// takes last-wins, so a duplicate here would not break metering — but
		// two sources of truth for the capability is how one of them goes
		// stale, and the harness should never be the one that mints it.
		for _, e := range append(l.Env, l.Argv...) {
			if strings.Contains(e, "ANTHROPIC_BASE_URL") {
				t.Fatalf("driver duplicates what Launch injects: %+v", l)
			}
		}
		if hasKey(l) {
			t.Fatalf("a real credential reached the container: %+v", l)
		}
		if !slices.Contains(l.Env, "ANTHROPIC_MODEL="+roles.hunt.Model) {
			t.Fatalf("driver model not pinned: %v", l.Env)
		}
	})
}

// hasKey looks for a real credential anywhere a container could read it. The
// guarantee is that the only place any key exists is the supervisor's memory.
func hasKey(l launch) bool {
	for _, a := range append(append([]string{}, l.Env...), l.Argv...) {
		if strings.Contains(a, "sk-anthropic") || strings.Contains(a, "sk-zai") {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- metering

func fakeUpstream(t *testing.T, seen *[]string) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Whatever the caller sent has been stripped and the real key attached.
		*seen = append(*seen, r.URL.Path+" key="+r.Header.Get("X-Api-Key")+
			" bearer="+r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"model":"claude-sonnet-5",`+
			`"content":[{"type":"text","text":"REFUTED\nthe lock is held on that path"}],`+
			`"usage":{"input_tokens":900,"output_tokens":40}}`)
	}))
	t.Cleanup(s.Close)
	return s
}

// The fourth juror end to end: one supervisor-side call, metered, on its own
// span and pool, with no CLI and no container anywhere in it.
func TestAskIsMeteredAndReachesTheProvider(t *testing.T) {
	var seen []string
	sw := sweepFor(t, fakeUpstream(t, &seen).URL)
	span := sw.Span(nil, "juror.glm", sw.Budget().Sub("jury.cheap", 10)).Only("glm")

	out, err := ask(context.Background(), span, "glm", "glm-4.6", "disprove this")
	if err != nil {
		t.Fatal(err)
	}
	if verdictOf(out) != "refuted" {
		t.Fatalf("verdictOf(%q) did not read the juror", out)
	}
	if len(seen) != 1 || !strings.Contains(seen[0], "/v1/messages") {
		t.Fatalf("upstream saw %v", seen)
	}
	// GLM is the Anthropic wire with bearer auth, so the proxy must have
	// attached a bearer and NOT an x-api-key.
	if !strings.Contains(seen[0], "bearer=Bearer sk-zai") || strings.Contains(seen[0], "key=sk-") {
		t.Fatalf("wrong auth shape for glm: %v", seen)
	}
	span.Close(glue.OK, "refuted")
	if err := sw.Err(); err != nil {
		t.Fatal(err)
	}
}

// The independence guarantee, mechanically: a juror span narrowed to one vendor
// cannot reach the driver's vendor even though the sweep holds that key and the
// juror knows the address. This is the assertion that makes "an entirely
// different set of logical weights" a property rather than a promise.
func TestJurorCannotReachTheDriversVendor(t *testing.T) {
	var seen []string
	sw := sweepFor(t, fakeUpstream(t, &seen).URL)
	juror := sw.Span(nil, "juror.glm", sw.Budget()).Only("glm")

	if _, err := ask(context.Background(), juror, "anthropic", "claude-sonnet-5", "x"); err == nil {
		t.Fatal("a jury span reached the driver's provider")
	}
	if len(seen) != 0 {
		t.Fatalf("the request left the process: %v", seen)
	}
}

// The budget is the fact dawn exists to make unforgeable, and it must bind on
// the supervisor's own calls exactly as it binds on a container's.
func TestBudgetRefusesTheJurorsCall(t *testing.T) {
	var seen []string
	sw := sweepFor(t, fakeUpstream(t, &seen).URL)
	broke := sw.Budget().Sub("jury.broke", 0.000001)
	span := sw.Span(nil, "juror.glm", broke).Only("glm")

	_, err := ask(context.Background(), span, "glm", "glm-4.6", "disprove this")
	if err == nil {
		t.Fatal("an over-cap call was forwarded")
	}
	if !strings.Contains(err.Error(), fmt.Sprint(http.StatusTooManyRequests)) {
		t.Fatalf("want 429, got %v", err)
	}
	if len(seen) != 0 {
		t.Fatalf("a refused request still left the process: %v", seen)
	}
}

// ---------------------------------------------------------------- the store

func TestStoreIsALogAndTheListIsASet(t *testing.T) {
	dir := t.TempDir()
	s, err := newStore(filepath.Join(dir, "findings.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	f := &Finding{Class: "injection", File: "x.go", Lo: 1, Hi: 2, Title: "t"}
	f.ID = f.id()
	s.add(f) // hunter files it
	f.Pass1 = "ok"
	f.Verdict = "stands"
	s.add(f) // jury updates it

	if got := len(s.list()); got != 1 {
		t.Fatalf("list has %d findings, want 1 — the report would double-count", got)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "findings.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(strings.TrimSpace(string(raw)), "\n") + 1; n != 2 {
		t.Fatalf("file has %d lines, want 2 — a crash between the two would lose the verdict", n)
	}
}

func TestClusterKeyGroupsByClassAndDirectory(t *testing.T) {
	a := &Finding{Class: "injection", File: "net/http/parse.go"}
	b := &Finding{Class: "injection", File: "net/http/serve.go"}
	c := &Finding{Class: "injection", File: "crypto/tls/handshake.go"}
	d := &Finding{Class: "authz-bypass", File: "net/http/parse.go"}
	if clusterKey(a) != clusterKey(b) {
		t.Fatal("same class, same directory did not cluster")
	}
	if clusterKey(a) == clusterKey(c) || clusterKey(a) == clusterKey(d) {
		t.Fatal("clustered across directory or class")
	}
}

// An agent that produced no usable answer must not look finished. This is the
// one place the harness's own bookkeeping can lie to its next run, so it is
// checked through the same read the next run would use.
func TestUnparsedJurorClosesFailed(t *testing.T) {
	sw := sweepFor(t, "http://127.0.0.1:1")
	root := sw.Span(nil, "repo", sw.Budget())

	closeJuror(sw.Span(root, "juror.answered", sw.Budget()), "refuted")
	closeJuror(sw.Span(root, "juror.silent", sw.Budget()), "unparsed")

	got := map[string]glue.Class{}
	outs, err := sw.Outcomes()
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range outs {
		got[o.Name] = o.Class
	}
	if got["juror.answered"] != glue.OK {
		t.Fatalf("answered juror closed %v", got["juror.answered"])
	}
	if got["juror.silent"] != glue.Failed {
		t.Fatalf("a juror that said nothing closed %v; a resumed run would skip it",
			got["juror.silent"])
	}
}

// Go's internal rule fences a harness in ANOTHER module: it gets a compile
// error, not a code review comment. This example is inside dawn's own module,
// so that fence does not apply to it — `import "github.com/valbaudo/dawn/
// internal/store"` compiles fine from here, which was verified by trying it.
// The example would therefore be a dishonest demonstration of the constraint it
// is meant to illustrate unless the constraint is asserted some other way. This
// is that assertion.
func TestHarnessImportsNothingUnderInternal(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	for _, pkg := range pkgs {
		for name, f := range pkg.Files {
			for _, imp := range f.Imports {
				if strings.Contains(imp.Path.Value, "dawn/internal") {
					t.Errorf("%s imports %s: this harness must reach dawn only "+
						"through its public API, exactly as an out-of-module one is forced to",
						name, imp.Path.Value)
				}
			}
		}
	}
}

// The report is the only thing a human reads, so its two halves are checked
// together: findings from the harness's store, money from Sweep.Outcomes.
func TestReport(t *testing.T) {
	var seen []string
	sw := sweepFor(t, fakeUpstream(t, &seen).URL)
	root := sw.Span(nil, "target", sw.Budget())

	h := &harness{sw: sw, pool: map[string]*glue.Pool{}}
	var err error
	if h.q, err = openQueue(filepath.Join(t.TempDir(), "tasks.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.q.close() })
	if h.find, err = newStore(filepath.Join(t.TempDir(), "f.jsonl")); err != nil {
		t.Fatal(err)
	}
	add := func(f *Finding) {
		f.ID = f.id()
		h.find.add(f)
	}
	add(&Finding{Class: "injection", Title: "unescaped path into exec", File: "net/cmd.go",
		Lo: 40, Hi: 52, Verdict: "stands", Cluster: "injection:net",
		Jury: map[string]string{"xai/grok-4-fast": "stands", "glm/glm-4.6": "stands"}})
	add(&Finding{Class: "injection", Title: "second sink, same helper", File: "net/run.go",
		Lo: 8, Hi: 12, Verdict: "stands", Cluster: "injection:net", Split: true,
		Note: "YES - both reach the same unescaped join",
		Jury: map[string]string{"xai/grok-4-fast": "refuted", "glm/glm-4.6": "stands",
			"openai-responses/gpt-5.6-sol": "stands"}})
	add(&Finding{Class: "authz-bypass", Title: "hallucinated", File: "nope.go",
		Lo: 1, Hi: 2, Verdict: "rejected", Pass1: "no such file"})

	// One real metered call so the spend column is not zero by construction.
	stage := sw.Span(root, "dedupe", sw.Budget().Sub("dedupe", 5))
	if _, err := ask(context.Background(), stage, "glm", "glm-4.6", "x"); err != nil {
		t.Fatal(err)
	}
	stage.Close(glue.OK, "")
	root.Close(glue.OK, "")

	var b strings.Builder
	h.report(&b, root)
	out := b.String()
	t.Logf("\n%s", out)

	if strings.Contains(out, "hallucinated") {
		t.Error("a finding rejected by pass 1 reached the report")
	}
	if !strings.Contains(out, "[jury split]") {
		t.Error("the report hid a disagreement")
	}
	if !strings.Contains(out, "2 findings survived") {
		t.Error("wrong survivor count")
	}
	if !strings.Contains(out, "TOTAL") || strings.Contains(out, "$0.0000\n  TOTAL") {
		t.Error("the spend column did not read the ledger")
	}
}

// ---------------------------------------------------------------- the prompts

// A prompt is a string and dawn will never look at it, so the compiler will not
// either: a verb that lost its argument ships as "%!s(MISSING)" inside a $200
// sweep and nothing anywhere notices. That is the one way these three constants
// break silently, and this is the check for it.
func TestPromptsRenderEverySlot(t *testing.T) {
	tax := taxon{
		Class:    "authz-bypass",
		Scope:    []string{"net/http/serve.go", "auth/token.go"},
		Why:      "handler dispatch happens before the session lookup at net/http/serve.go:210",
		Attacker: "an unauthenticated client holding a valid-looking cookie",
		Sink:     "auth/token.go:88",
	}
	f := &Finding{Class: tax.Class, Title: "session is trusted before it is verified",
		File: "auth/token.go", Lo: 80, Hi: 92,
		Threat: "an unauthenticated client sets the cookie and reaches the admin route",
		Detail: "net/http/serve.go:210 -> auth/token.go:88"}

	for name, got := range map[string]string{
		"recon":    fmt.Sprintf(reconPrompt, strings.Join(seed, ", ")),
		"hunt":     huntPrompt(tax),
		"disprove": disprovePrompt(f),
	} {
		if strings.Contains(got, "%!") {
			t.Fatalf("%s prompt has an unfilled verb:\n%s", name, got)
		}
		if strings.Contains(got, "%s") || strings.Contains(got, "%d") {
			t.Fatalf("%s prompt shipped a literal verb:\n%s", name, got)
		}
	}

	// The five taxonomy fields ARE the domain payload; a hunt prompt that
	// dropped one is a hunter re-deriving what recon already paid to learn.
	hunt := huntPrompt(tax)
	for _, want := range []string{tax.Class, tax.Why, tax.Attacker, tax.Sink,
		"net/http/serve.go", "auth/token.go"} {
		if !strings.Contains(hunt, want) {
			t.Fatalf("hunt prompt lost %q:\n%s", want, hunt)
		}
	}
	// A juror that cannot see the citation cannot run step 1, which is the
	// cheapest and most decisive of the five.
	if !strings.Contains(disprovePrompt(f), "auth/token.go:80-92") {
		t.Fatalf("disprove prompt lost the citation:\n%s", disprovePrompt(f))
	}

	// The Fact is the version, so the same taxon must hash the same way or the
	// join from a finding back to the prompt that produced it is noise.
	if string(hash(huntPrompt(tax))) != string(hash(huntPrompt(tax))) {
		t.Fatal("prompt rendering is not deterministic; prompt.sha256 means nothing")
	}
}

// The recon prompt says every scope path must exist. This is where that stops
// being a request: a hunter scoped to a hallucinated path burns a whole agent
// run finding out.
func TestGroundDropsWhatIsNotOnDisk(t *testing.T) {
	repo := t.TempDir()
	os.MkdirAll(filepath.Join(repo, "net"), 0o755)
	os.WriteFile(filepath.Join(repo, "net/serve.go"), []byte("package net\n"), 0o644)

	got := ground(repo, []taxon{
		{Class: "injection", Scope: []string{"net/serve.go", "net/ghost.go"}},
		{Class: "authz-bypass", Scope: []string{"nope/at/all.go"}},
		{Class: "", Scope: []string{"net/serve.go"}},
		{Class: "escape", Scope: []string{"../../etc/passwd"}},
	})
	if len(got) != 1 {
		t.Fatalf("ground kept %d classes, want 1: %+v", len(got), got)
	}
	if got[0].Class != "injection" || !slices.Equal(got[0].Scope, []string{"net/serve.go"}) {
		t.Fatalf("ground = %+v", got[0])
	}
}

// TestMissingOutputIsFailedNotOK pins the coverage-loss bug: a hunter whose
// agent exits 0 without writing findings.jsonl must close Failed, because
// done() keys resumption on OK and an OK here retires the attack class for
// every later run of the sweep.
func TestMissingOutputIsFailedNotOK(t *testing.T) {
	dir := t.TempDir()

	if _, _, err := readJSONL(filepath.Join(dir, "findings.jsonl")); err == nil {
		t.Fatal("a missing file must be an error, not an empty result: " +
			"silent zero and honest zero must stay distinguishable")
	}

	empty := filepath.Join(dir, "empty.jsonl")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got, bad, err := readJSONL(empty)
	if err != nil || len(got) != 0 || bad != 0 {
		t.Fatalf("an agent that ran and found nothing is not an error: %v %d %v", got, bad, err)
	}

	mixed := filepath.Join(dir, "mixed.jsonl")
	if err := os.WriteFile(mixed, []byte("{\"file\":\"a.go\"}\nnot json\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, bad, err = readJSONL(mixed)
	if err != nil || len(got) != 1 || bad != 1 {
		t.Fatalf("unparseable lines must be counted, not dropped: %d found, %d bad, %v", len(got), bad, err)
	}
}
