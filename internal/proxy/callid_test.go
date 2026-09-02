package proxy

import (
	"net/http"
	"testing"
	"time"
)

// ---- call_id: the three gaps the outcome-string rule could not see ---------

// A 500 stamped `x-should-retry: false` is permanent. _should_retry checks that
// header before it looks at the status code, so the SDK will NOT resend — and
// an identical body arriving later is a new logical call, not attempt 2.
func TestShouldRetryFalseOnA500SplitsTheCall(t *testing.T) {
	f := newFixture(t, 100, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Should-Retry", "false")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(500)
		w.Write([]byte(`{"type":"error","error":{"type":"api_error","message":"nope"}}`))
	})
	post(t, f.base(), streamReq).Body.Close()
	post(t, f.base(), streamReq).Body.Close()

	rs := f.waitRows(2)
	if len(rs) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rs))
	}
	if rs[0].CallID == rs[1].CallID {
		t.Fatalf("x-should-retry:false was ignored: both attempts got call_id %s", rs[0].CallID)
	}
	if rs[1].Attempt != 1 {
		t.Fatalf("second call should be attempt 1, got %d", rs[1].Attempt)
	}
}

// The mirror image: a 400 stamped `x-should-retry: true`. The status code says
// permanent, the header says resend, and the header wins.
func TestShouldRetryTrueOnA400MergesTheCall(t *testing.T) {
	f := newFixture(t, 100, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Should-Retry", "true")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"x"}}`))
	})
	post(t, f.base(), streamReq).Body.Close()
	post(t, f.base(), streamReq).Body.Close()

	rs := f.waitRows(2)
	if len(rs) != 2 || rs[0].CallID != rs[1].CallID {
		t.Fatalf("header-forced retry did not merge: %+v", rs)
	}
	if rs[1].Attempt != 2 {
		t.Fatalf("want attempt 2, got %d", rs[1].Attempt)
	}
}

// 409 conflict_error. _should_retry retries it ("Retry on lock timeouts"), and
// the old outcome-string list did not contain it.
func TestConflictIsRetryable(t *testing.T) {
	if !willRetry(409, http.Header{}) {
		t.Fatal("409 must be retryable: _should_retry retries it explicitly")
	}
	for _, s := range []int{408, 429, 500, 502, 529} {
		if !willRetry(s, http.Header{}) {
			t.Fatalf("%d must be retryable", s)
		}
	}
	for _, s := range []int{200, 400, 401, 402, 403, 404, 413} {
		if willRetry(s, http.Header{}) {
			t.Fatalf("%d must not be retryable", s)
		}
	}
}

// The window. An agent that fails a call, then spends twenty minutes compiling
// and running a fuzz target, then resends the identical body is starting a new
// logical call — the SDK gave up long ago. Without a clock the old rule welded
// them together and COUNT(DISTINCT call_id) under-reported forever.
func TestRetryWindowExpires(t *testing.T) {
	f := newFixture(t, 100, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(529)
		w.Write([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"o"}}`))
	})
	post(t, f.base(), streamReq).Body.Close()
	f.waitRows(1)

	// Age the recorded attempt past the window. last_seen is what identify
	// measures from, which is exactly why it is the column it reads.
	old := time.Now().Add(-retryWindow - time.Second).UnixMilli()
	if _, err := f.db.SQL().Exec(`UPDATE events SET last_seen=? WHERE kind='call'`, old); err != nil {
		t.Fatal(err)
	}

	post(t, f.base(), streamReq).Body.Close()
	rs := f.waitRows(2)
	if len(rs) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rs))
	}
	if rs[0].CallID == rs[1].CallID {
		t.Fatal("a retryable failure older than retryWindow must not be joined")
	}
}

// Inside the window the same failure does merge — the window narrows the rule,
// it does not disable it.
func TestRetryInsideWindowStillMerges(t *testing.T) {
	f := newFixture(t, 100, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(529)
		w.Write([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"o"}}`))
	})
	post(t, f.base(), streamReq).Body.Close()
	f.waitRows(1)
	post(t, f.base(), streamReq).Body.Close()

	rs := f.waitRows(2)
	if len(rs) != 2 || rs[0].CallID != rs[1].CallID || rs[1].Attempt != 2 {
		t.Fatalf("529 inside the window must merge as attempt 2: %+v", rs)
	}
}

// A refused budget must not cost three round trips. We mint the 429 ourselves,
// so we also get to tell the SDK not to bother.
func TestBudgetRefusalTellsTheSDKNotToRetry(t *testing.T) {
	f := newFixture(t, 0, func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be reached when the budget refuses")
	})
	res := post(t, f.base(), streamReq)
	defer res.Body.Close()
	if got := res.Header.Get("X-Should-Retry"); got != "false" {
		t.Fatalf("want x-should-retry: false on a budget refusal, got %q", got)
	}
}
