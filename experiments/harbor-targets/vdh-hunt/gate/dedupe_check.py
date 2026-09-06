"""Dedupe gate: every distinct finding kept once, and nothing invented.

Fully checkable, so it is a sound gate rather than a shape check:

    subset    every kept finding was in the input
    unique    no (file, function) appears twice in the output
    complete  every distinct (file, function) in the input survives

That last clause is what stops "dedupe" degenerating into "delete". Collapsing
duplicates is the job; losing a finding is not, and the difference is exactly
countable.
"""
import json
import pathlib

KEPT = pathlib.Path("/app/outputs/deduped.json")
INPUTS = pathlib.Path("/app/inputs")
LOGS = pathlib.Path("/logs/verifier")
METRICS = {"in": 0, "distinct_in": 0, "out": 0}


def fail(reason):
    print("REJECT: " + reason)
    return 0


def main():
    seen = []
    for f in sorted(INPUTS.glob("*/*.json")):
        try:
            d = json.loads(f.read_text())
        except Exception:
            continue
        for item in (d if isinstance(d, list) else d.get("findings", [d])):
            if isinstance(item, dict) and isinstance(item.get("file"), str) \
               and isinstance(item.get("function"), str):
                seen.append((item["file"], item["function"]))
    if not seen:
        # Nothing was handed in. That is dawn's wiring, not the agent's work.
        import sys
        sys.exit("no verdict: no findings were mounted for this stage")
    distinct = set(seen)
    METRICS.update({"in": len(seen), "distinct_in": len(distinct)})

    if not KEPT.exists():
        return fail(f"{KEPT} does not exist")
    try:
        doc = json.loads(KEPT.read_text())
    except Exception as e:
        return fail(f"{KEPT} is not JSON: {e}")
    out = doc.get("findings") if isinstance(doc, dict) else None
    if not isinstance(out, list):
        return fail("no 'findings' list")

    keys = []
    for item in out:
        if not isinstance(item, dict) or not isinstance(item.get("file"), str) \
           or not isinstance(item.get("function"), str):
            return fail("every kept finding needs a string file and function")
        keys.append((item["file"], item["function"]))
    METRICS["out"] = len(keys)
    print(f"in={len(seen)} distinct={len(distinct)} out={len(keys)}")

    if len(keys) != len(set(keys)):
        return fail("the output still contains duplicates")
    invented = set(keys) - distinct
    if invented:
        return fail(f"invented {len(invented)} finding(s) that were never handed in: {sorted(invented)}")
    lost = distinct - set(keys)
    if lost:
        return fail(f"dropped {len(lost)} distinct finding(s): {sorted(lost)}")
    print("PROVEN: every distinct finding kept exactly once, nothing invented")
    return 1


reward = main()
print(f"VERDICT: {reward}")
LOGS.mkdir(parents=True, exist_ok=True)
(LOGS / "reward.json").write_text(json.dumps({"reward": reward, **METRICS}))
