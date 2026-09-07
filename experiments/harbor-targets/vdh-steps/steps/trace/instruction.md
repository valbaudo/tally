Your input is the deduped findings. Read /app/src, including the request
handlers in src/api.py, which are the outside edge.

For EACH finding, give the call path by which attacker-controlled input reaches
it, starting at a handler and ending at the vulnerable function itself. Every
consecutive pair must be a call this source actually makes - the verifier parses
the code and checks each hop.

Write one JSON object:
  {"traces": [{"function": "...", "path": ["handler", "...", "the function"], "why": "..."}]}

Write it to /app/outputs/trace.json
