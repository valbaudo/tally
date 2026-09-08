"""Trace gate: is the bug reachable, and is the path real?

VDH's trace stage asks whether "attacker-controlled input actually reaches the
bug from outside the system". A claimed path is checkable without believing a
word of the reasoning: the gate parses its OWN copy of the source and confirms
every hop is a call edge that exists.

    path[0]              is a real function
    path[i] -> path[i+1] is a call this source actually makes
    path[-1]             is the finding's vulnerable function

So a plausible-sounding path through functions that never call each other is
refused, and tally is reading an AST rather than prose.
"""
import ast
import json
import pathlib
import sys

TRACE = pathlib.Path("/app/outputs/trace.json")
INPUTS = pathlib.Path("/app/inputs")
SRC = pathlib.Path("/gate/src")
LOGS = pathlib.Path("/logs/verifier")
METRICS = {"traced": 0, "hops": 0}

assert len(list(SRC.glob("*.py"))) >= 5, f"the gate's own copy of the target is missing from {SRC}"


def call_graph():
    """function name -> set of function names it calls, over the whole target."""
    edges, defined = {}, set()
    for path in sorted(SRC.glob("*.py")):
        tree = ast.parse(path.read_text())
        for node in ast.walk(tree):
            if isinstance(node, ast.FunctionDef):
                defined.add(node.name)
                out = set()
                for sub in ast.walk(node):
                    if isinstance(sub, ast.Call):
                        f = sub.func
                        out.add(f.id if isinstance(f, ast.Name) else
                                getattr(f, "attr", None))
                edges[node.name] = {o for o in out if o}
    return edges, defined


def fail(reason):
    print("REJECT: " + reason)
    return 0


def main():
    findings = sorted(INPUTS.glob("*/*.json"))
    wanted = set()
    for f in findings:
        try:
            d = json.loads(f.read_text())
        except Exception:
            continue
        for item in (d.get("findings", []) if isinstance(d, dict) else []):
            if isinstance(item, dict) and isinstance(item.get("function"), str):
                wanted.add(item["function"])
    if not wanted:
        sys.exit("no verdict: no deduped findings were mounted for this stage")

    if not TRACE.exists():
        return fail(f"{TRACE} does not exist")
    try:
        doc = json.loads(TRACE.read_text())
    except Exception as e:
        return fail(f"{TRACE} is not JSON: {e}")
    traces = doc.get("traces") if isinstance(doc, dict) else None
    if not isinstance(traces, list):
        return fail("no 'traces' list")

    edges, defined = call_graph()
    seen = set()
    hops = 0
    for t in traces:
        if not isinstance(t, dict):
            return fail("a trace is not an object")
        fn, path = t.get("function"), t.get("path")
        if not isinstance(fn, str) or not isinstance(path, list) or not path:
            return fail("every trace needs a string function and a non-empty path")
        if any(not isinstance(h, str) for h in path):
            return fail("a path hop is not a string")
        if path[-1] != fn:
            return fail(f"the path for {fn} ends at {path[-1]!r}, not at the function itself")
        for hop in path:
            if hop not in defined:
                return fail(f"{hop!r} is not a function in this target")
        for a, b in zip(path, path[1:]):
            if b not in edges.get(a, set()):
                return fail(f"{a} does not call {b}: that path does not exist in this source")
        hops += len(path) - 1
        seen.add(fn)
    METRICS.update({"traced": len(seen), "hops": hops})
    print(f"traced {len(seen)}/{len(wanted)} findings over {hops} verified call edges")

    missing = wanted - seen
    if missing:
        return fail(f"no reachability path for: {sorted(missing)}")
    print("PROVEN: every finding has a path whose every hop is a real call edge")
    return 1


reward = main()
print(f"VERDICT: {reward}")
LOGS.mkdir(parents=True, exist_ok=True)
(LOGS / "reward.json").write_text(json.dumps({"reward": reward, **METRICS}))
