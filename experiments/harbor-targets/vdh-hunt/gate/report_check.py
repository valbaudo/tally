"""Report gate: the hunt's coverage, scored against ground truth.

This is the one stage that holds the answer key, and it is the right one: VDH's
report stage is where recall and precision live, and the toy carries four
seeded instances of one bug class plus four safe siblings precisely so that
"flag everything" scores worse than recognising the class.

reward 1 iff recall == 1 AND precision == 1: all four found, no decoy flagged.
Partial coverage is a real result and is reported as metrics, not as a pass --
a protocol reads them through Result.Metric.
"""
import json
import pathlib
import sys

REPORT = pathlib.Path("/app/outputs/report.json")
TRUTH = json.loads(pathlib.Path("/gate/ground_truth.json").read_text())
LOGS = pathlib.Path("/logs/verifier")

VULN = {(v["file"], v["function"]) for v in TRUTH["vulnerable"]}
METRICS = {"recall": 0.0, "precision": 0.0, "flagged": 0, "true_positives": 0}


def fail(reason):
    print("REJECT: " + reason)
    return 0


def main():
    if not REPORT.exists():
        return fail(f"{REPORT} does not exist")
    try:
        doc = json.loads(REPORT.read_text())
    except Exception as e:
        return fail(f"{REPORT} is not JSON: {e}")
    findings = doc.get("findings") if isinstance(doc, dict) else None
    if not isinstance(findings, list):
        return fail("the report has no 'findings' list")

    flagged = set()
    for f in findings:
        if not isinstance(f, dict):
            return fail("a finding is not an object")
        rel, name = f.get("file"), f.get("function")
        if not isinstance(rel, str) or not isinstance(name, str):
            return fail("every finding needs a string file and function")
        flagged.add((rel, name))

    hits = flagged & VULN
    recall = len(hits) / len(VULN)
    precision = len(hits) / len(flagged) if flagged else 0.0
    METRICS.update({"recall": recall, "precision": precision,
                    "flagged": len(flagged), "true_positives": len(hits)})
    for f, n in sorted(VULN):
        print(f"  {'FOUND  ' if (f, n) in flagged else 'MISSED '} {f}:{n}")
    for f, n in sorted(flagged - VULN):
        print(f"  FALSE   {f}:{n}")
    print(f"recall={recall:.2f} precision={precision:.2f} flagged={len(flagged)}")

    if recall < 1.0:
        return fail(f"the hunt missed {len(VULN) - len(hits)} of {len(VULN)}")
    if precision < 1.0:
        return fail(f"{len(flagged) - len(hits)} finding(s) are not the seeded class")
    return 1


reward = main()
print(f"VERDICT: {reward}")
LOGS.mkdir(parents=True, exist_ok=True)
(LOGS / "reward.json").write_text(json.dumps({"reward": reward, **METRICS}))
