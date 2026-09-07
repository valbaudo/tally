Your input is ONE hunter's finding. Your job is to DISPROVE it.

Read /app/src and attack the claim: is the input really attacker-controlled, does
it really reach SQL text unsanitized, does the payload really change the query's
meaning rather than merely returning rows?

You may not report a finding of your own. Your verdict is about THIS finding and
this one only, and the verifier refuses a verdict naming anything else.

Write one JSON object, echoing the file and function you were given:
  {"file": "...", "function": "...", "verdict": "confirmed" | "refuted", "why": "..."}

Write it to /app/outputs/verdict.json
