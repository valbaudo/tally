The Python package at /app/src is a small data-access layer with request
handlers on top. Read all of it.

Produce a hunting queue: one entry per area worth examining, each naming the
module and the functions in it that build SQL, with a one-line note on how each
gets its query text.

Do not decide yet which are vulnerable - this stage maps the ground.

Write one JSON object:
  {"queue": [{"file": "src/....py", "functions": ["..."], "note": "..."}]}

Write it to /app/outputs/map.json
