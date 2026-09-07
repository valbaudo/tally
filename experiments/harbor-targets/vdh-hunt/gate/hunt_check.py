"""Hunt gate: the finding names a function and a payload, and the gate runs it.

reward 1 iff, against a database this image built after the agent finished:

    f(conn, payload)  returns a canary row      -- the query's meaning changed
    f(conn, benign)   does not                  -- and the payload is why

No ground truth is consulted here, deliberately. A gate that checked the cited
function against a list of known-vulnerable functions would be a FORMAT check
wearing an oracle's clothes: it would pass an agent that guessed the name and
never wrote a working payload, and it could never find a bug the list omits.
Ground truth belongs to the report stage, which scores coverage, not proof.
"""
import json
import pathlib
import sys

sys.path.insert(0, "/gate")
from oracle import call, canary, database, load, patched

POV = pathlib.Path("/app/outputs/finding.json")
INPUTS = pathlib.Path("/app/inputs")
LOGS = pathlib.Path("/logs/verifier")
BENIGN = "zzz-matches-nothing"
METRICS = {"proven": 0, "threat_model": 0, "fix_flips": 0, "on_queue": 0}


def fail(reason):
    print("REJECT: " + reason)
    return 0


def main():
    if not POV.exists():
        return fail(f"{POV} does not exist")
    try:
        f = json.loads(POV.read_text())
    except Exception as e:
        return fail(f"{POV} is not JSON: {e}")
    if not isinstance(f, dict):
        return fail("the finding is not an object")

    rel, name, payload = f.get("file"), f.get("function"), f.get("payload")
    if not all(isinstance(x, str) for x in (rel, name, payload)):
        return fail("a finding needs string file, function and payload")
    if not payload:
        return fail("the payload is empty")

    # The threat model, schema-checked. Cloudflare's harness requires a
    # finding to name "attacker identity, boundary crossed" alongside its PoC;
    # a payload that works while nobody can say WHO would send it or WHAT it
    # crosses is a curiosity, not a finding.
    threat = f.get("threat")
    if not isinstance(threat, dict):
        return fail("a finding needs a 'threat' object")
    for k in ("attacker", "boundary"):
        if not isinstance(threat.get(k), str) or len(threat[k].strip()) < 12:
            return fail(f"threat.{k} must say something: got {threat.get(k)!r}")
    METRICS["threat_model"] = 1

    mod = load(rel)
    if mod is None:
        return fail(f"{rel!r} is not a module in this target")
    fn = getattr(mod, name, None)
    if not callable(fn):
        return fail(f"{rel}:{name} is not a callable in this target")
    print(f"CITED: {rel}:{name} with payload {payload!r}")

    # THE ENTRY IT WAS SENT TO. The queue is the hunter's assignment, and until
    # now nothing read it: the prose said "take ENTRY i" and the gate could not
    # tell whether the hunter had. Measured in vdh-20260907T153150Z it had not.
    # All three round-2 hunters were handed a queue naming four untouched
    # functions, and all three went back to one confirmed two rounds earlier,
    # so six of nine hunt attempts and their six paired adversaries bought
    # nothing. A prose instruction the gate cannot see is a request the agent
    # may decline, and this one was declined every time it mattered.
    #
    # This is a SCOPE check, not an oracle, and that distinction is the one this
    # module's header draws. The queue is the agent side's own plan, written by
    # an ungated stage. It is not ground truth and says nothing about which
    # functions are really weak, so consulting it cannot become the format
    # check wearing an oracle's clothes that the header warns against. It asks
    # only "did you hunt where you were sent", exactly as the validate gate
    # asks "is this verdict about the finding you were given".
    #
    # What it does NOT do, said plainly so the metric is not over-read: it
    # cannot stop two hunters choosing the SAME entry. This gate sees one
    # attempt and never its siblings, and a hunter's own number reaches it
    # through no channel the hunter cannot alter. Round 1 of that same run,
    # where all three hunters took entry 0, still passes.
    entries = []
    for q in sorted(INPUTS.glob("*/map.json")):
        try:
            entries += json.loads(q.read_text()).get("queue", [])
        except Exception:
            continue
    if not entries:
        # dawn wiring, not the agent's doing: there is nothing to check against,
        # and a gate that cannot check must not vote.
        sys.exit("no verdict: no hunting queue was mounted for this stage")

    at = f.get("queue_entry")
    if not isinstance(at, int) or isinstance(at, bool) or not 0 <= at < len(entries):
        return fail(f"queue_entry {at!r} is not an index into the {len(entries)}-entry queue")
    sent = entries[at]
    if rel != sent.get("file"):
        return fail(f"queue entry {at} sent you to {sent.get('file')!r}, and this finding is in {rel!r}")
    named = sent.get("functions") or []
    # An entry naming no functions is the whole file, which recon and gapfill
    # both emit for a module they want swept rather than probed.
    if named and name not in named:
        return fail(f"queue entry {at} named {named}, and this finding is about {name!r}")
    METRICS["on_queue"] = 1
    print(f"ON QUEUE: entry {at} sent this hunter to {rel}, and that is where the finding is")

    mark = str(canary())
    exploit = call(fn, database(int(mark)), payload)
    control = call(fn, database(int(mark)), BENIGN)
    METRICS.update({"exploit_cols": len(exploit.split()), "control_cols": len(control.split())})

    if mark not in exploit:
        return fail(f"the payload did not reach a canary row through {name}")
    if mark in control:
        return fail(f"{name} returns the canary for ANY input: the payload is not what did it")

    METRICS["proven"] = 1
    print(f"PROVEN: {name} returned a row the payload had no right to reach")

    # THE FIX, and its flip. A finding that cannot say how to close the hole is
    # half a finding, and a proposed fix is the most checkable thing in the
    # whole artifact: apply it to the gate's OWN copy and replay the very same
    # payload. Cloudflare's Fixer demands "a clean fail→pass flip on the target
    # test"; this is that, with the exploit as the test.
    fix = f.get("fix")
    if not isinstance(fix, dict) or not all(isinstance(fix.get(k), str) for k in ("old", "new")):
        return fail("a finding needs a 'fix' with string 'old' and 'new'")
    if not fix["old"].strip():
        return fail("the fix replaces nothing")
    root, reason = patched(rel, fix["old"], fix["new"])
    if reason:
        return fail(reason)
    fixed = getattr(load(rel, root=root) or object, name, None)
    if not callable(fixed):
        return fail("the fixed module does not import, or no longer defines that function")
    mark = str(canary())
    if mark in call(fixed, database(int(mark)), payload):
        return fail("the payload still reaches the canary after the fix: it closes nothing")
    if not call(fixed, database(int(mark)), BENIGN) and call(fn, database(int(mark)), BENIGN):
        return fail("the fix breaks the function for ordinary input")
    METRICS["fix_flips"] = 1
    print("FIXED: the same payload no longer reaches it, and ordinary input still works")
    return 1


reward = main()
print(f"VERDICT: {reward}")
# Last act, unconditional. If the gate died before it -- its own source
# missing, a broken interpreter -- there is no verdict at all, which is
# infra_error and not a 0. A gate that could not check must not vote.
LOGS.mkdir(parents=True, exist_ok=True)
(LOGS / "reward.json").write_text(json.dumps({"reward": reward, **METRICS}))
