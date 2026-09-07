Your inputs are the confirmed findings and their reachability paths.

A bug found in one place is a hunting task everywhere the same shape could
occur. From what was confirmed, emit a queue aimed at the places NOT yet
confirmed - same class, different call site, or the same handler reached a
different way.

Write one JSON object:
  {"queue": [{"file": "src/....py", "functions": ["..."], "note": "what pattern sent you here"}]}

Write it to /app/outputs/map.json
