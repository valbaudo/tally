package store

import (
	"path/filepath"
	"sync"
	"testing"
)

func TestReserveIsAtomicAndFailsClosed(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "g.db"), "s1")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.NewPool("sweep", "", 1.00, 0.50); err != nil {
		t.Fatal(err)
	}
	if err := d.NewPool("task", "sweep", 100.00, 0); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	ok, refused := 0, 0
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				n, err := d.Reserve("task", 0.001)
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				if n == 2 {
					ok++
				} else if n == 0 {
					refused++
				} else {
					t.Errorf("partial chain: %d", n)
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	sp, cp, sv, _ := d.Spent("sweep")
	tsp, _, _, _ := d.Spent("task")
	t.Logf("ok=%d refused=%d sweep.spent=%v cap=%v salvage=%v task.spent=%v", ok, refused, sp, cp, sv, tsp)
	if float64(sp) > 0.50 {
		t.Fatalf("SAFETY: crossed effective cap: %v", sp)
	}
	if float64(sp)+0.001 <= 0.50 {
		t.Fatalf("TIGHTNESS: left room: %v", sp)
	}
	if sp != tsp {
		t.Fatalf("child charged without parent: %v vs %v", tsp, sp)
	}

	claimed, err := d.Claim("sweep")
	if err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	if again, _ := d.Claim("sweep"); again {
		t.Fatal("double claim reported true")
	}
	if n, _ := d.Reserve("task", 0.001); n != 2 {
		t.Fatal("salvage did not widen the cap")
	}

	// TrueUp may exceed cap; next reserve must refuse.
	if err := d.TrueUp("task", 5.0); err != nil {
		t.Fatal(err)
	}
	if n, _ := d.Reserve("task", 0.001); n != 0 {
		t.Fatal("cap did not bind after overspend")
	}
	sp, _, _, _ = d.Spent("sweep")
	if float64(sp) < 5.0 {
		t.Fatalf("true-up was clamped: %v", sp)
	}
}

func TestSweepLockAndReap(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(filepath.Join(dir, "g.db"), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(filepath.Join(dir, "g.db"), "s1"); err == nil {
		t.Fatal("second supervisor on the same sweep was allowed in")
	}
	if d2, err := Open(filepath.Join(dir, "g.db"), "s2"); err != nil {
		t.Fatalf("concurrent sweep refused: %v", err)
	} else {
		d2.Close()
	}

	for _, s := range []string{"a", "b"} {
		if err := d.Append(Row{Span: s, Kind: "span_open", Outcome: "task"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.Append(Row{Span: "a", Kind: "span_close", Class: OK}); err != nil {
		t.Fatal(err)
	}
	n, err := d.Reap()
	if err != nil || n != 1 {
		t.Fatalf("reap: %d %v (want 1)", n, err)
	}
	if n, _ := d.Reap(); n != 0 {
		t.Fatalf("reap not idempotent: %d", n)
	}
	if err := d.Append(Row{Span: "a", Kind: "span_open"}); err == nil {
		t.Fatal("duplicate span_open accepted; the two-rows invariant is not enforced")
	}
	d.Close()
}
