package api

// Mechanical checks for the claims this prototype keeps getting wrong.
//
// Five passes asserted invariants in prose and shipped violations anyway:
// "called everywhere" (four of eight files), "all four" (two of four),
// "one clock model" (wrong quantity at the leaf). Every claim that was
// checked by a script held; every claim merely stated did not.
//
// So the bar stops being narrative. These tests fail the build when an
// invariant breaks, which is the only form of the bar that has worked.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// protocolFiles are the four protocols plus the ports. port_test.go is in the
// list because skipping it is exactly how the last pass failed.
var protocolFiles = []string{
	"proto_cybergym.go", "proto_mdash.go", "proto_vdh.go", "proto_prci.go", "port_test.go",
}

func readAll(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	names = append(names, "TRACE.md")
	for _, n := range names {
		b, err := os.ReadFile(n)
		if err != nil {
			t.Fatalf("read %s: %v", n, err)
		}
		out[n] = string(b)
	}
	return out
}

// TestNoHandWrittenDispatchClock: a dispatching scope's WallClock must come
// from Dispatching, never from a hand-written literal. Dispatching derives it
// as attempts x perAttempt, so an under-clocked scope cannot be expressed;
// this guards the one way back in.
func TestNoHandWrittenDispatchClock(t *testing.T) {
	src := readAll(t)
	for _, f := range protocolFiles {
		for i, line := range strings.Split(src[f], "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if strings.Contains(line, "AttemptWallClock:") {
				t.Errorf("%s:%d hand-writes AttemptWallClock (%q); use Dispatching(attempts, perAttempt) so the clock is derived",
					f, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// TestDispatchingDerivesTheFloor pins the derivation itself.
func TestDispatchingDerivesTheFloor(t *testing.T) {
	for _, c := range []struct {
		attempts int
		per      time.Duration
	}{{2, 10 * time.Minute}, {8, 12 * time.Minute}, {32, 20 * time.Minute}, {10, 30 * time.Minute}} {
		l := Dispatching(c.attempts, c.per)
		if want := time.Duration(c.attempts) * c.per; l.WallClock != want {
			t.Errorf("Dispatching(%d, %v).WallClock = %v, want the serial floor %v", c.attempts, c.per, l.WallClock, want)
		}
	}
}

// TestOneVotePredicate: "did anything come back from the work" is defined once,
// in State.Decided. Four passes re-derived it inline with a different spelling
// each time and each pass fixed one site and missed another.
func TestOneVotePredicate(t *testing.T) {
	banned := []*regexp.Regexp{
		regexp.MustCompile(`observed\s*=\s*true`),
		regexp.MustCompile(`!=\s*InfraError`),
		regexp.MustCompile(`len\(found\)\s*>\s*0`),
	}
	src := readAll(t)
	for _, f := range protocolFiles {
		for i, line := range strings.Split(src[f], "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			for _, re := range banned {
				if re.MatchString(line) {
					t.Errorf("%s:%d re-derives the vote predicate inline (%q); call State.Decided or anyDecided", f, i+1, strings.TrimSpace(line))
				}
			}
		}
	}
}

// TestTraceAttributionsAreTrue: TRACE.md must attribute every construct to a
// named protocol or a named settled decision, and every attribution must be
// true by grep. Three false attributions have shipped, including one written
// by the pass whose purpose was to fix false attributions.
func TestTraceAttributionsAreTrue(t *testing.T) {
	src := readAll(t)
	trace := src["TRACE.md"]
	// The Cut table's "Forced by" column names who WOULD have used a deleted
	// construct, which is a different claim; only the kept table is checked.
	if i := strings.Index(trace, "Why deleting it would break a decision"); i > 0 {
		if j := strings.Index(trace[i:], "\n## "); j > 0 {
			trace = trace[:i+j]
		}
	}
	if i := strings.Index(trace, "## Cut"); i > 0 {
		trace = trace[:i]
	}

	// Every row that claims a protocol forced a construct must name a protocol
	// whose file actually mentions that construct.
	fileOf := map[string]string{
		"cybergym": "proto_cybergym.go",
		"mdash":    "proto_mdash.go",
		"vdh":      "proto_vdh.go",
		"pr-ci":    "proto_prci.go",
	}
	rowRe := regexp.MustCompile("(?m)^\\| `([^`]+)` [^|]*\\| ([^|]*) \\|")
	for _, m := range rowRe.FindAllStringSubmatch(trace, -1) {
		construct, forced := m[1], m[2]
		parts := regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`).FindAllString(construct, -1)
		if len(parts) == 0 {
			continue
		}
		// The construct's own name: the first identifier, except for a
		// method row like `State.Decided()` where it is the second.
		ident := parts[0]
		if len(parts) > 1 && (ident == "State" || ident == "Scope" || ident == "Result") {
			ident = parts[1]
		}
		if ident == "" {
			continue
		}
		claimsAll := strings.Contains(forced, "all four")
		for name, file := range fileOf {
			claimed := claimsAll || strings.Contains(forced, name)
			if !claimed {
				continue
			}
			if !strings.Contains(src[file], ident) {
				t.Errorf("TRACE row %q claims %q forced it, but %s never mentions %q",
					construct, name, file, ident)
			}
		}
	}
}

// TestNoConstructIsUnattributed: an exported identifier in api.go with no
// TRACE.md row survives by omission, which the Cut table itself calls out.
func TestNoConstructIsUnattributed(t *testing.T) {
	src := readAll(t)
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "api.go", src["api.go"], 0)
	if err != nil {
		t.Fatal(err)
	}
	trace := src["TRACE.md"]
	var missing []string
	ast.Inspect(f, func(n ast.Node) bool {
		switch d := n.(type) {
		case *ast.TypeSpec:
			if d.Name.IsExported() && !strings.Contains(trace, d.Name.Name) {
				missing = append(missing, "type "+d.Name.Name)
			}
		case *ast.FuncDecl:
			if d.Name.IsExported() && !strings.Contains(trace, d.Name.Name) {
				missing = append(missing, "func "+d.Name.Name)
			}
		}
		return true
	})
	for _, m := range missing {
		t.Errorf("api.go exports %s but TRACE.md has no row for it", m)
	}
}
