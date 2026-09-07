Your inputs are under /app/inputs, one directory per stage that produced them:
the hunting queue a round worked from, and the adversaries' verdicts on what
that round found. Read /app/src as well.

A finding an adversary REFUTED leaves its area still uncovered - treat it as a
gap, not as ground already walked.

Find the GAP: functions or modules the round touched but did not cover, and
anything no hunter looked at. Emit the queue for the next round, aimed only at
what is still unexamined.

Write one JSON object:
  {"queue": [{"file": "src/....py", "functions": ["..."], "note": "why this is still open"}]}

Write it to /app/outputs/map.json
