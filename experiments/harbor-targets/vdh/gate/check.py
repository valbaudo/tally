"""vdh gate: a CITATION / FORMAT check, not an oracle.

Passing means the findings are well-formed and grounded in the real source.
It does NOT mean they are the ground-truth bugs -- this gate has never seen
the ground truth and cannot judge correctness.
"""
import json
import pathlib

FINDINGS = pathlib.Path("/app/outputs/findings.jsonl")
REPO = pathlib.Path("/gate/repo")
FILES = ["src/db.py", "src/users.py", "src/orders.py", "src/search.py", "src/reports.py"]
# Loaded at import, outside any try, on purpose: if this image's own source is
# missing the gate dies here and writes no reward -- infra_error, not a 0.
SOURCE = {f: (REPO / f).read_text().splitlines() for f in FILES}


def norm(s):
    return " ".join(s.split())


def check(rec):
    """Return None if the finding is well-formed and grounded, else a reason."""
    if not isinstance(rec, dict):
        return "not a JSON object"
    for k in ("file", "line", "class", "evidence"):
        if k not in rec:
            return f"missing key {k!r}"
    path = str(rec["file"]).strip()
    for prefix in ("/app/repo/", "./"):
        if path.startswith(prefix):
            path = path[len(prefix):]
    if path not in SOURCE:
        return f"file {rec['file']!r} is not a file in this repo"
    if not isinstance(rec["line"], int) or isinstance(rec["line"], bool):
        return f"line {rec['line']!r} is not an integer"
    lines = SOURCE[path]
    if not 1 <= rec["line"] <= len(lines):
        return f"line {rec['line']} out of range for {path} (1..{len(lines)})"
    cited = lines[rec["line"] - 1]
    stripped = cited.strip()
    if len(stripped) < 8 or stripped.startswith("#"):
        return f"{path}:{rec['line']} is a trivial line: {cited!r}"
    for k in ("class", "evidence"):
        if not isinstance(rec[k], str) or not rec[k].strip():
            return f"{k} is empty or not a string"
    if norm(rec["evidence"]) not in norm(cited):
        return f"evidence not verbatim at {path}:{rec['line']}; that line is {cited!r}"
    return None


def main():
    if not FINDINGS.exists():
        print(f"FAIL: {FINDINGS} does not exist (no findings is a failure, not 'zero findings')")
        return 0
    raw = [ln for ln in FINDINGS.read_text().splitlines() if ln.strip()]
    print(f"findings.jsonl: {len(raw)} non-empty line(s)")
    if not raw:
        print("FAIL: findings.jsonl is empty")
        return 0
    ok = True
    for i, ln in enumerate(raw, 1):
        try:
            rec = json.loads(ln)
        except json.JSONDecodeError as e:
            print(f"  line {i}: REJECT - not valid JSON ({e})")
            ok = False
            continue
        reason = check(rec)
        if reason:
            print(f"  line {i}: REJECT - {reason}")
            ok = False
        else:
            print(f"  line {i}: OK   {rec['file']}:{rec['line']} class={rec['class']!r}")
    return 1 if ok else 0


reward = main()
print(f"VERDICT: {reward} (well-formed + grounded citations; NOT a correctness oracle)")
# LAST act, unconditional, numbers only. Harbor reads reward.json before
# reward.txt; writing it last is what makes the verdict unforgeable. A gate
# that dies before this line is an infra_error, not a verdict -- so there is
# no reward.txt fallback anywhere.
pathlib.Path("/logs/verifier").mkdir(parents=True, exist_ok=True)
pathlib.Path("/logs/verifier/reward.json").write_text(json.dumps({"reward": reward}))
