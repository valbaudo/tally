Your inputs are under /app/inputs, one directory per hunter that produced a
finding. Some describe the SAME bug.

Collapse them, and DECLARE the collapsing: each finding you keep lists the
inputs it stands for, including itself. Two reports of one bug become one
finding absorbing both - even where they name different functions, if you judge
them the same bug.

The verifier checks your grouping is a partition: every input absorbed by
exactly one keeper, nothing absorbed that was never handed in. It does not
second-guess WHICH things you judged equivalent - but it will not let work be
silently dropped.

Write one JSON object:
  {"findings": [{"file": "src/....py", "function": "...", "why": "...",
                 "absorbed": [{"file": "...", "function": "..."}]}]}

Write it to /app/outputs/deduped.json
