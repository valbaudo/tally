package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	glue "github.com/valbaudo/dawn"
)

func testQueue(t *testing.T) *queue {
	t.Helper()
	q, err := openQueue(filepath.Join(t.TempDir(), "tasks.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { q.close() })
	return q
}

// The property the whole design rests on: the guard and the write are one
// statement, so two workers cannot hold one cell. Everything else about a queue
// is convenience; this one is money — a double claim is two containers billing
// the same work.
func TestOneWorkerPerCell(t *testing.T) {
	q := testQueue(t)
	const cells = 200
	for i := range cells {
		if err := q.push(key(i), []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
	}

	var mu sync.Mutex
	seen := map[string]int{}
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				k, _, _, err := q.take()
				if errors.Is(err, sql.ErrNoRows) {
					return
				}
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				seen[k]++
				mu.Unlock()
				q.done(k, "done")
			}
		}()
	}
	wg.Wait()

	if len(seen) != cells {
		t.Fatalf("%d cells were handed out, want %d", len(seen), cells)
	}
	for k, n := range seen {
		if n != 1 {
			t.Fatalf("%s was claimed %d times: two agents billed one cell", k, n)
		}
	}
}

// A crash costs the cell in flight and nothing else. There is no lease column
// and no TTL: the sweep's flock is the lease, so every claim in the file at
// open belonged to a process that is gone. Reopening is the reap.
func TestCrashRequeuesTheCellInFlightAndNothingElse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tasks.db")
	q, err := openQueue(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"a", "b", "c"} {
		if err := q.push(k, []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	first, _, _, _ := q.take()
	q.done(first, "done")
	inFlight, _, _, err := q.take() // claimed, never retired: the crash
	if err != nil {
		t.Fatal(err)
	}
	q.close()

	q2, err := openQueue(path) // the next supervisor boots
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { q2.close() })

	got := map[string]string{}
	for _, row := range mustList(t, q2) {
		got[row.Key] = row.State
	}
	if got[first] != "done" {
		t.Fatalf("a finished cell was requeued: %v", got)
	}
	if got[inFlight] != "queued" {
		t.Fatalf("the cell in flight was not requeued: %v", got)
	}
	// And it comes back with its attempt counted, which is what makes a poison
	// cell visible instead of eternal.
	for {
		k, _, tries, err := q2.take()
		if err != nil {
			t.Fatal(err)
		}
		if k == inFlight {
			if tries != 2 {
				t.Fatalf("requeued cell reports %d tries, want 2", tries)
			}
			return
		}
	}
}

// Cloudflare's Feedback "instantly rewrites queued prompts". The word that
// carries the semantics is QUEUED: a rewrite must not race a worker whose
// request is already in flight, and a cell that is finished must not come back.
func TestFeedbackRewritesOnlyQueuedCells(t *testing.T) {
	q := testQueue(t)
	// take() is ORDER BY key, so the names fix the order: retire the first,
	// leave the second claimed, leave the third untouched.
	cells := []string{"a.finished", "b.running", "c.queued"}
	for _, k := range cells {
		if err := q.push(k, []byte(`"v1"`)); err != nil {
			t.Fatal(err)
		}
	}
	k, _, _, err := q.take()
	if err != nil || k != "a.finished" {
		t.Fatalf("take = %q, %v", k, err)
	}
	q.done(k, "done")
	if k, _, _, err := q.take(); err != nil || k != "b.running" {
		t.Fatalf("take = %q, %v", k, err)
	}

	for _, k := range cells {
		if err := q.push(k, []byte(`"v2"`)); err != nil {
			t.Fatal(err)
		}
	}

	want := map[string]string{"c.queued": `"v2"`, "b.running": `"v1"`, "a.finished": `"v1"`}
	for k, w := range want {
		var got string
		if err := q.db.QueryRow(`SELECT payload FROM task WHERE key = ?`, k).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != w {
			t.Fatalf("%s payload = %s, want %s", k, got, w)
		}
	}
	// And the finished cell stays finished, which is why Gapfill can re-offer
	// the whole cross-product without redoing work.
	var state string
	if err := q.db.QueryRow(`SELECT state FROM task WHERE key = 'a.finished'`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "done" {
		t.Fatalf("a finished cell was re-offered: state = %q", state)
	}
}

// A cell that kills the supervisor comes back through the boot requeue. Without
// a cap it comes back forever, at full concurrency, and the invoice is the
// first symptom.
func TestPoisonCellStopsComingBack(t *testing.T) {
	q := testQueue(t)
	if err := q.push("poison", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	for i := range maxTries {
		if _, _, _, err := q.take(); err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
		if _, err := q.db.Exec(`UPDATE task SET state='queued' WHERE key='poison'`); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, _, err := q.take(); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("a cell was claimed a %dth time: %v", maxTries+1, err)
	}
	n, err := q.queued()
	if err != nil || n != 0 {
		t.Fatalf("queued() still counts a poison cell: %d %v", n, err)
	}
}

// Gapfill is the only reason this harness owns a queue: it authors work that
// did not exist when the run began. The rule is one line — a (class, path) cell
// with no finding citing it is thin — and it covers both the hunter that ran
// and found nothing there and the hunter whose whole class failed.
func TestGapfillEnqueuesThinCellsOnly(t *testing.T) {
	h := &harness{q: testQueue(t)}
	var err error
	if h.find, err = newStore(filepath.Join(t.TempDir(), "f.jsonl")); err != nil {
		t.Fatal(err)
	}
	tax := []taxon{
		{Class: "injection", Scope: []string{"net", "db"}, Why: "w", Attacker: "a", Sink: "s"},
		{Class: "authz-bypass", Scope: []string{"auth"}},
	}
	h.find.add(&Finding{ID: "1", Class: "injection", File: "net/cmd.go"})
	// A finding of the WRONG class under db/ must not mark db covered for
	// injection, or one lucky hunter retires a cell nobody hunted.
	h.find.add(&Finding{ID: "2", Class: "authz-bypass", File: "db/conn.go"})

	if n := h.gapfill(tax); n != 2 {
		t.Fatalf("gapfill enqueued %d cells, want 2 (injection/db and authz-bypass/auth)", n)
	}
	got := map[string]bool{}
	for _, row := range mustList(t, h.q) {
		got[row.Key] = true
	}
	for _, want := range []string{"gap.injection:db", "gap.authz-bypass:auth"} {
		if !got[want] {
			t.Fatalf("thin cell %s was not enqueued: %v", want, got)
		}
	}
	if got["gap.injection:net"] {
		t.Fatalf("a covered cell was re-hunted: %v", got)
	}

	// The payload is the taxon narrowed to the one thin path — the sharpening
	// IS the narrowing, because scope is what makes a hunter's context budget
	// achievable. And it is five typed fields, so the next rewrite diffs.
	_, payload, _, err := h.q.take()
	if err != nil {
		t.Fatal(err)
	}
	var cell taxon
	if err := json.Unmarshal(payload, &cell); err != nil {
		t.Fatal(err)
	}
	if len(cell.Scope) != 1 {
		t.Fatalf("gap cell kept the whole scope: %v", cell.Scope)
	}
	if cell.Why == "" && cell.Class == "injection" {
		t.Fatal("gap cell dropped the domain payload recon paid to produce")
	}
}

func TestUnderIsAPathTestNotAPrefixTest(t *testing.T) {
	cases := []struct {
		file, scope string
		want        bool
	}{
		{"net/cmd.go", "net", true},
		{"net/cmd.go", "net/cmd.go", true},
		{"./net/deep/cmd.go", "net", true},
		{"network/cmd.go", "net", false}, // the bug a HasPrefix would ship
		{"net.go", "net", false},
	}
	for _, c := range cases {
		if got := under(c.file, c.scope); got != c.want {
			t.Fatalf("under(%q, %q) = %v", c.file, c.scope, got)
		}
	}
}

// ---------------------------------------------------------------- the registry

// Every column in the registry that claims a mechanism must have one. This is
// the test that keeps `role` from decaying into documentation.
func TestRolesEnforceWhatTheyDeclare(t *testing.T) {
	sw := sweepFor(t, "http://127.0.0.1:1")
	span := sw.Span(nil, "t", sw.Budget())
	h := &harness{sw: sw, image: map[string]string{
		"claude": "img/claude", "codex": "img/codex", "droid": "img/droid"}}

	var share float64
	for _, r := range allRoles() {
		share += r.Share
		if r.Provider == "" || r.Model == "" || r.Name == "" {
			t.Fatalf("role %+v is missing an enforced column", r)
		}
		if r.CLI == "" {
			continue // called straight from the supervisor; no container to check
		}
		l, err := h.agentLaunch(r, span, t.TempDir(), "p")
		if err != nil {
			t.Fatalf("%s: %v", r.Name, err)
		}
		if l.Image != h.image[r.CLI] {
			t.Fatalf("%s got image %q, not its CLI's", r.Name, l.Image)
		}
		if len(r.Writes) == 0 {
			t.Fatalf("%s runs a container and names no artifact; nothing would be read", r.Name)
		}
		// ReadOnly is one bit in the registry and three flags in the world.
		// A role that declares it and does not carry its CLI's spelling is a
		// must-not with no mechanism, which is the thing this file is against.
		argv := strings.Join(l.Argv, " ")
		flag := map[string]string{
			"claude": `--tools ""`,
			"codex":  "--sandbox read-only",
			"droid":  "--disabled-tools Create,Edit,ApplyPatch",
		}[r.CLI]
		if got := strings.Contains(argv, flag); got != r.ReadOnly {
			t.Fatalf("%s: ReadOnly=%v but argv %s the flag %q:\n%s",
				r.Name, r.ReadOnly, map[bool]string{true: "carries", false: "lacks"}[got], flag, argv)
		}
	}
	if share > 1.0001 {
		t.Fatalf("the roles claim %.2f of the sweep cap", share)
	}

	// Heterogeneity is the point, and it is a property of the registry rather
	// than of any prompt: no juror may sit on the vendor that produced the
	// finding it is judging.
	for _, j := range []role{roles.cheapA, roles.cheapB, roles.heavy} {
		if j.Provider == roles.hunt.Provider {
			t.Fatalf("juror %s shares the hunter's vendor; the jury is a rubber stamp", j.Name)
		}
	}
}

// Span.Only is the mechanism behind the registry's Provider column, and it is
// enforced by the proxy rather than by anything the agent is told. Assert it
// through the public API the harness actually uses.
func TestOpenNarrowsTheSpanToTheRolesVendor(t *testing.T) {
	sw := sweepFor(t, "http://127.0.0.1:1")
	h := &harness{sw: sw, work: t.TempDir(), pool: map[string]*glue.Pool{}}
	for _, r := range allRoles() {
		h.pool[r.Name] = sw.Budget().Sub(r.Name, 1)
	}
	span, _, err := h.open(nil, roles.cheapB, roles.cheapB.Name, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ask(context.Background(), span, roles.hunt.Provider, roles.hunt.Model, "x"); err == nil {
		t.Fatal("a juror span reached the hunter's vendor")
	}
}

func key(i int) string { return "cell-" + strings.Repeat("0", 3-len(itoa(i))) + itoa(i) }

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

func mustList(t *testing.T, q *queue) []task {
	t.Helper()
	rows, err := q.list()
	if err != nil {
		t.Fatal(err)
	}
	return rows
}
