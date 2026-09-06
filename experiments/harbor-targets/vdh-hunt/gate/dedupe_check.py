"""Dedupe gate: the grouping must be a partition. Collapsing is the JOB.

WHAT THIS GOT WRONG, because it is worth naming: the first version treated
(file, function) as the unit and refused any output that did not keep every
distinct pair. Cloudflare's Dedupe collapses "semantically equivalent
findings" — two hunters describing ONE bug at different call sites are one
finding — so that check did not merely miss the job, it FORBADE it. An agent
that correctly recognised two reports as the same bug was failed for "dropping"
one.

The error was letting what is easy to verify decide what the stage means. A
grouping is just as checkable as an identity, as long as the agent DECLARES it:
each kept finding lists the inputs it absorbed, and the gate checks the
declaration is a well-formed partition of what was handed in.

    covering   every input finding is absorbed by exactly one kept finding
    honest     nothing absorbed was ever invented
    grounded   each kept finding is itself one of the inputs it absorbed

Semantic collapsing passes. Inventing a finding fails. Losing one fails. What
the agent may NOT do is quietly drop work, and that is the only thing this
stage was ever supposed to prevent.
"""
import json
import pathlib
import sys

KEPT = pathlib.Path("/app/outputs/deduped.json")
INPUTS = pathlib.Path("/app/inputs")
LOGS = pathlib.Path("/logs/verifier")
METRICS = {"in": 0, "kept": 0, "collapsed": 0}


def fail(reason):
    print("REJECT: " + reason)
    return 0


def key(d):
    return (d.get("file"), d.get("function"))


def main():
    handed = set()
    for f in sorted(INPUTS.glob("*/*.json")):
        try:
            d = json.loads(f.read_text())
        except Exception:
            continue
        for item in (d if isinstance(d, list) else d.get("findings", [d])):
            if isinstance(item, dict) and all(isinstance(item.get(k), str) for k in ("file", "function")):
                handed.add(key(item))
    if not handed:
        sys.exit("no verdict: no findings were mounted for this stage")
    METRICS["in"] = len(handed)

    if not KEPT.exists():
        return fail(f"{KEPT} does not exist")
    try:
        doc = json.loads(KEPT.read_text())
    except Exception as e:
        return fail(f"{KEPT} is not JSON: {e}")
    out = doc.get("findings") if isinstance(doc, dict) else None
    if not isinstance(out, list):
        return fail("no 'findings' list")

    claimed = {}
    for item in out:
        if not isinstance(item, dict) or not all(isinstance(item.get(k), str) for k in ("file", "function")):
            return fail("every kept finding needs a string file and function")
        absorbed = item.get("absorbed")
        if not isinstance(absorbed, list) or not absorbed:
            return fail(f"{key(item)} declares no 'absorbed' list: say which inputs it stands for")
        group = set()
        for a in absorbed:
            if not isinstance(a, dict) or not all(isinstance(a.get(k), str) for k in ("file", "function")):
                return fail("every absorbed entry needs a string file and function")
            group.add(key(a))
        if key(item) not in group:
            return fail(f"{key(item)} does not absorb itself: a representative must be one of the findings it stands for")
        for g in group:
            if g not in handed:
                return fail(f"{key(item)} absorbs {g}, which was never handed in")
            if g in claimed:
                return fail(f"{g} is absorbed by both {claimed[g]} and {key(item)}: that is not a partition")
            claimed[g] = key(item)
        print(f"  {key(item)[0]}:{key(item)[1]}  stands for {len(group)}")

    missing = handed - set(claimed)
    if missing:
        return fail(f"{len(missing)} finding(s) absorbed by nobody: {sorted(missing)}")
    METRICS.update({"kept": len(out), "collapsed": len(handed) - len(out)})
    print(f"PROVEN: {len(handed)} findings partitioned into {len(out)}, "
          f"{METRICS['collapsed']} collapsed, none invented or lost")
    return 1


reward = main()
print(f"VERDICT: {reward}")
LOGS.mkdir(parents=True, exist_ok=True)
(LOGS / "reward.json").write_text(json.dumps({"reward": reward, **METRICS}))
