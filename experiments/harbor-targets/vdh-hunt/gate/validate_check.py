"""Validate gate: was the adversary RIGHT?

VDH's validator "cannot log findings of its own; its sole job is to
aggressively disprove the Hunter's theory". Two things follow, and this gate
enforces both.

It may not invent. Its verdict must be about the finding it was handed --
same file, same function -- so a validator that wanders off and reports
something else is refused before anything is executed.

And its verdict is checkable. The hunt oracle is right here: the gate replays
the payload itself against a freshly planted canary. So

    verdict "confirmed" is right iff the payload really proves
    verdict "refuted"   is right iff it really does not

That makes this stage a measurement of the ADVERSARY, not of the finding. A
validator that rubber-stamps everything fails on the refutable ones; one that
refutes everything fails on the real ones. Neither can pass by having an
opinion.
"""
import json
import pathlib
import sys

sys.path.insert(0, "/gate")
from oracle import call, canary, database, load

VERDICT = pathlib.Path("/app/outputs/verdict.json")
INPUTS = pathlib.Path("/app/inputs")
LOGS = pathlib.Path("/logs/verifier")
BENIGN = "zzz-matches-nothing"
METRICS = {"agreed": 0, "proves": 0}


def fail(reason):
    print("REJECT: " + reason)
    return 0


def proves(rel, name, payload):
    mod = load(rel)
    fn = getattr(mod, name, None) if mod else None
    if not callable(fn):
        return False
    mark = str(canary())
    return mark in call(fn, database(int(mark)), payload) and \
        mark not in call(fn, database(int(mark)), BENIGN)


def main():
    findings = sorted(INPUTS.glob("*/finding.json"))
    if len(findings) != 1:
        # The stage is dispatched with exactly one finding to attack. More or
        # fewer is dawn wiring the protocol wrong, not the agent's doing, so
        # there is nothing here to score.
        sys.exit(f"no verdict: expected exactly one input finding, found {len(findings)}")
    given = json.loads(findings[0].read_text())

    if not VERDICT.exists():
        return fail(f"{VERDICT} does not exist")
    try:
        v = json.loads(VERDICT.read_text())
    except Exception as e:
        return fail(f"{VERDICT} is not JSON: {e}")
    if not isinstance(v, dict):
        return fail("the verdict is not an object")

    call_ = v.get("verdict")
    if call_ not in ("confirmed", "refuted"):
        return fail(f"verdict {call_!r} is not one of: confirmed, refuted")

    # "Cannot log findings of its own": the verdict must be about the finding
    # it was handed, and about nothing else.
    if (v.get("file"), v.get("function")) != (given.get("file"), given.get("function")):
        return fail(f"the verdict is about {v.get('file')}:{v.get('function')}, "
                    f"not the finding it was given ({given.get('file')}:{given.get('function')})")

    truth = proves(given.get("file", ""), given.get("function", ""), given.get("payload", ""))
    METRICS["proves"] = int(truth)
    agreed = (call_ == "confirmed") == truth
    METRICS["agreed"] = int(agreed)
    print(f"finding {given.get('file')}:{given.get('function')} — "
          f"machine says {'PROVES' if truth else 'does not prove'}, adversary said {call_}")
    if not agreed:
        return fail("the adversary was wrong about a finding the gate can settle by running it")
    print("PROVEN: the adversary's verdict matches what the payload actually does")
    return 1


reward = main()
print(f"VERDICT: {reward}")
LOGS.mkdir(parents=True, exist_ok=True)
(LOGS / "reward.json").write_text(json.dumps({"reward": reward, **METRICS}))
