package dawn

import (
	"strings"
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
}

// LiveGate is a sound gate, not a format-only one: it must report "live" from
// kind() (not fall through to "sound", which would make its receipt and its
// report indistinguishable from a gate that never touched a live host), and
// it must still be capable of Passed, which formatOnly forecloses.
func TestLiveGateKindAndFormatOnly(t *testing.T) {
	g := LiveGate(Image("g@sha256:"+strings.Repeat("a", 64)), "target.example.com")
	if got := g.kind(); got != "live" {
		t.Errorf("LiveGate.kind() = %q, want %q", got, "live")
	}
	if g.formatOnly {
		t.Error("LiveGate must not be format-only")
	}
}

// contentDigest must NOT move when a stage's gate hosts change — hosts are
// the engagement scope, not attempt identity (LiveGate's own comment says
// why). This is the test that proves adding Gate.hosts did not silently
// re-key every existing dedup and resume record keyed on attemptID.
func TestContentDigestIgnoresGateHosts(t *testing.T) {
	img := Image("g@sha256:" + strings.Repeat("c", 64))
	sound := Stage{ID: "s", Agent: ClaudeCode, Env: "e@sha256:0", Prompt: "do it", Gate: SoundGate(img)}
	live := sound
	live.Gate = LiveGate(img, "target.example.com")
	if contentDigest(sound) != contentDigest(live) {
		t.Error("contentDigest moved when only Gate.hosts changed: hosts must not be folded into attempt identity")
	}
}

// contentDigest must NOT move when only Agent.model or Agent.effort differs
// — contentDigest's own comment says why: they are enforced by resumeResult
// instead, as the sixth field alongside the hash rather than folded into it.
func TestContentDigestIgnoresModelAndEffort(t *testing.T) {
	base := Stage{ID: "s", Agent: ClaudeCode, Env: "e@sha256:0", Prompt: "do it"}
	pinnedDifferently := base
	pinnedDifferently.Agent = Agent{name: ClaudeCode.name, image: ClaudeCode.image, model: "some-other-model", effort: "high"}
	if contentDigest(base) != contentDigest(pinnedDifferently) {
		t.Error("contentDigest moved when only Agent.model/effort changed: they must not be folded into attempt identity")
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
	// The stage id is hashed OUTSIDE contentDigest, but still feeds attemptID.
	differentID := base
	differentID.ID = "other"
	if attemptID(base, 1) == attemptID(differentID, 1) {
		t.Error("attemptID must vary with stage.ID")
	}
}
