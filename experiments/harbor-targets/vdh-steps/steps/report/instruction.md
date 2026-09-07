Your inputs are the confirmed findings, their reachability paths, and the
follow-up queue. Read /app/src yourself.

Produce the final report: the functions really vulnerable to the seeded class -
user input reaching SQL text through string building instead of a bound
parameter.

PRECISION IS SCORED, not just recall. Every vulnerable function here has a safe
sibling doing the analogous read with a bound parameter, and the request
handlers build no SQL at all. Listing everything scores worse than listing what
you can defend.

Write one JSON object:
  {"findings": [{"file": "src/....py", "function": "...", "why": "..."}]}

Write it to /app/outputs/report.json
