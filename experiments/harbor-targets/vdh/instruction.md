A small Python repo is checked out at `/app/repo`. It talks to sqlite3
through one shared helper, `src/db.py`.

Audit it for the class of bug where **untrusted input is spliced into SQL
text instead of being passed as a bound `?` parameter**. Read the source;
you may run it (`python3 /app/repo/demo_exploits.py`) if that helps.

Write your findings to `/app/outputs/findings.jsonl`: **one JSON object per line**,
each with exactly these keys:

```json
{"file": "src/users.py", "line": 12, "class": "sql-injection", "evidence": "sql = f\"SELECT id, name, is_admin FROM users WHERE name = '{username}'\""}
```

- `file` — path relative to the repo root, e.g. `src/orders.py`.
- `line` — 1-based line number in that file. It must be the line that
  actually builds the SQL string.
- `class` — a short label for the bug class.
- `evidence` — a **verbatim** snippet copied from that exact line.
  Whitespace is normalized, nothing else: if the snippet is not on the line
  you cited, the finding is rejected.

Report only what you can cite. The file must not be empty.
