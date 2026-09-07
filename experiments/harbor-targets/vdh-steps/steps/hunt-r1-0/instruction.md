You are hunter 1 of 3 in a hunt over the Python package at /app/src.

Your input is the hunting queue. Take ENTRY 0 from its "queue" list (zero-based)
and hunt what it names. If the queue is shorter than that, take an area no other
entry covers.

You hunt ONE class: user input reaching SQL text through string building -
f-string, %-formatting, .format(), or concatenation - instead of a bound
parameter.

Hand back ONE function with that flaw and a payload demonstrating it. Your
finding is judged by RUNNING it: the verifier calls the function you name with
the payload you give and accepts only if a row comes back that your payload had
no right to reach, and only if the same function with a harmless payload does
not return it. Try it yourself first.

Note the request handlers in src/api.py take a request mapping, not a bare
string - the bug is one hop further in, and that is where a finding belongs.

A finding is not just a payload. Hand back all four parts:

  {"file": "src/....py", "function": "...", "payload": "...",
   "why":    "how the input reaches the SQL text",
   "threat": {"attacker": "who can send this", "boundary": "what it crosses"},
   "fix":    {"old": "the exact source text to replace, verbatim",
              "new": "what replaces it"}}

The verifier applies your fix to ITS OWN copy and replays the same payload: it
must no longer reach anything, and ordinary input must still work. "old" has to
appear exactly once in the file, so include enough lines to be unambiguous.

Write it to /app/outputs/finding.json
