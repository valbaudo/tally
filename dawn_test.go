package dawn

import (
	"testing"
	"time"
)

// The one thing the prototype got wrong: a dispatching scope's clock must fund
// the sleeping as well as the running, or a scope that spends its counter on
// infra_error retries runs out of clock before it runs out of attempts.
func TestDispatchingFundsBackoff(t *testing.T) {
	for _, n := range []int{1, 2, 8, 80} {
		l := Dispatching(n, 20*time.Minute)
		serial := time.Duration(n) * 20 * time.Minute
		if l.Attempts != n || l.AttemptWallClock != 20*time.Minute {
			t.Fatalf("n=%d: %+v", n, l)
		}
		if l.WallClock < serial {
			t.Fatalf("n=%d: clock %v below serial floor %v", n, l.WallClock, serial)
		}
		if n > 1 && l.WallClock == serial {
			t.Fatalf("n=%d: clock is exactly the serial floor, funds no backoff", n)
		}
	}
	// Clamped exponential: 15s, 30s, 1m, 2m, 4m, then 5m forever.
	if got, want := retryBackoffTotal(6), 15*time.Second+30*time.Second+time.Minute+2*time.Minute+4*time.Minute; got != want {
		t.Fatalf("retryBackoffTotal(6) = %v, want %v", got, want)
	}
	if got, want := retryBackoffTotal(8)-retryBackoffTotal(6), 2*retryBackoffCap; got != want {
		t.Fatalf("gaps past the cap = %v, want %v", got, want)
	}
}

func TestDecided(t *testing.T) {
	for s, want := range map[State]bool{
		Passed: true, Rejected: true, Unverified: true,
		Exhausted: false, InfraError: false, Cancelled: false, State(""): false,
	} {
		if s.Decided() != want {
			t.Errorf("%q.Decided() = %v", s, !want)
		}
	}
	if !anyDecided([]Result{{State: InfraError}, {State: Rejected}}) {
		t.Error("anyDecided missed a rejected child")
	}
	if anyDecided([]Result{{State: InfraError}, {State: Exhausted}}) {
		t.Error("anyDecided voted on a fan where nothing voted")
	}
}

// attempt_id is stable for the same attempt and moves when any of its three
// stated ingredients does — stage content, resolved inputs, or attempt
// number — because a later ticket silently changing what feeds it is exactly
// the failure the loud comment on attemptID warns about.
func TestAttemptIDIsStableAndSensitiveToWhatItHashes(t *testing.T) {
	base := Stage{
		ID: "s", Agent: ClaudeCode, Env: "e@sha256:0", Prompt: "do it",
		Outputs: []string{"out"}, Gate: SoundGate("g@sha256:0"),
	}
	if attemptID(base, 1) != attemptID(base, 1) {
		t.Error("attemptID is not deterministic for identical inputs")
	}
	if attemptID(base, 1) == attemptID(base, 2) {
		t.Error("attemptID must vary with the attempt number")
	}
	differentPrompt := base
	differentPrompt.Prompt = "do it differently"
	if attemptID(base, 1) == attemptID(differentPrompt, 1) {
		t.Error("attemptID must vary with stage content (contentDigest)")
	}
	withInput := base
	withInput.Inputs = []Result{{Manifest: Manifest{{Name: "a", Digest: "sha256:1"}}}}
	if attemptID(base, 1) == attemptID(withInput, 1) {
		t.Error("attemptID must vary with resolved inputs (inputDigest)")
	}
	// The stage id is hashed OUTSIDE contentDigest, but still feeds attemptID.
	differentID := base
	differentID.ID = "other"
	if attemptID(base, 1) == attemptID(differentID, 1) {
		t.Error("attemptID must vary with stage.ID")
	}
}
