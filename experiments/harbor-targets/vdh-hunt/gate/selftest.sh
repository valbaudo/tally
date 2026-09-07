#!/bin/bash
# Build-time proof that this image honours the contract, run as the last layer.
# Both gates live in both images (only /tests/test.sh differs), so one selftest
# covers both. Everything here is offline: the oracle is a sqlite database this
# image builds for itself.
set -uo pipefail
A=/app/outputs

want() { python3 "$1" >/dev/null 2>&1
         r=$(python3 -c 'import json;print(json.load(open("/logs/verifier/reward.json"))["reward"])' 2>/dev/null)
         raw=$(cat /logs/verifier/reward.json 2>/dev/null); rm -rf /logs
         [ "$r" = "$2" ] || { echo "SELFTEST FAILED: $3 -> ${raw:-no reward.json}"; exit 1; }; }
# A gate that cannot check must not vote.
abstain() { python3 "$1" >/dev/null 2>&1; r=$(ls /logs/verifier/reward.json 2>/dev/null); rm -rf /logs
            [ -z "$r" ] || { echo "SELFTEST FAILED: $2 -> wrote reward.json, expected none"; exit 1; }; }
H=/gate/hunt_check.py
R=/gate/report_check.py
mkdir -p $A

# The hunting queue the gate now reads. A hunter is SENT to one entry, and the
# gate refuses a finding somewhere else -- the same shape as the validate
# gate's "is this verdict about the finding you were given". Entry 0 names both
# users.py functions so the decoy case below still fails for the reason its
# label claims (a payload that exploits nothing) rather than for being off
# queue. Entry 2 names no functions, which is how recon and gapfill say "sweep
# this whole module".
mkdir -p /app/inputs/recon
cat > /app/inputs/recon/map.json <<'Q'
{"queue": [{"file": "src/users.py", "functions": ["get_user_by_name", "get_user_by_id"]},
           {"file": "src/orders.py", "functions": ["search_orders_by_status"]},
           {"file": "src/search.py", "functions": []}]}
Q

# ---- hunt gate: grounded + proven + threat model + a fix that flips ----
# A complete finding is written by this helper; each case below breaks exactly
# one part of it, so a failure names the part.
finding() { python3 - "$@" > $A/finding.json <<'P'
import json, sys
f = {"file": sys.argv[1], "function": sys.argv[2], "payload": sys.argv[3],
     "queue_entry": int(sys.argv[6]) if len(sys.argv) > 6 else 0,
     "why": "user input is spliced into the SQL text instead of bound",
     "threat": {"attacker": "any caller who can reach the request handler",
                "boundary": "the database query's meaning, crossed by unescaped input"},
     "fix": {"old": sys.argv[4], "new": sys.argv[5]}}
json.dump(f, open("/app/outputs/finding.json", "w"))
P
}
UOLD=$(cat <<'P'
    sql = f"SELECT id, name, is_admin FROM users WHERE name = '{username}'"
    return run_query(conn, sql)
P
)
UNEW='    return run_query(conn, "SELECT id, name, is_admin FROM users WHERE name = ?", (username,))'
INJ="nobody' OR '1'='1"

rm -f $A/finding.json
want $H 0 "nothing at $A/finding.json"
echo 'not json' > $A/finding.json
want $H 0 "the finding is not JSON"
echo '{}' > $A/finding.json
want $H 0 "the finding names no function"
echo '{"file":"src/nope.py","function":"x","payload":"y"}' > $A/finding.json
want $H 0 "the cited module is not in this target"

# The threat model is part of the contract, not decoration.
python3 - "$INJ" <<'P'
import json, sys
json.dump({"file": "src/users.py", "function": "get_user_by_name",
           "payload": sys.argv[1], "queue_entry": 0},
          open("/app/outputs/finding.json", "w"))
P
want $H 0 "a working payload with no threat model"

# THE FORGERIES. The source is in the agent's image, so a function NAME is free
# to guess; a payload that changes a query's meaning is not, and neither is a
# fix that actually closes it.
finding "src/users.py" "get_user_by_name" "alice" "$UOLD" "$UNEW"
want $H 0 "a real vulnerable function named with a payload that exploits nothing"
finding "src/users.py" "get_user_by_id" "$INJ" "$UOLD" "$UNEW"
want $H 0 "a decoy named with a real injection payload"
finding "src/users.py" "get_user_by_name" "$INJ" "    return run_query(conn, sql)" "    return run_query(conn, sql)  # reviewed"
want $H 0 "a fix that changes nothing the payload cares about"
finding "src/users.py" "get_user_by_name" "$INJ" "def get_user_by_name(conn, username):" "def get_user_by_name(conn, username):  # noqa"
want $H 0 "a fix that edits a signature and leaves the hole open"

# THE QUEUE, which is the assignment. Everything below proves a real payload
# and a flipping fix, so the only thing left to fail on is where it hunted.
finding "src/users.py" "get_user_by_name" "$INJ" "$UOLD" "$UNEW" 1
want $H 0 "a proven finding declaring an entry that sent it to another file"
finding "src/users.py" "get_user_by_name" "$INJ" "$UOLD" "$UNEW" 99
want $H 0 "an entry index that is not in the queue"
python3 - "$INJ" <<'P'
import json, sys
json.dump({"file": "src/users.py", "function": "get_user_by_name", "payload": sys.argv[1],
           "why": "x", "threat": {"attacker": "any caller who can reach it",
                                  "boundary": "the query's meaning, crossed by input"},
           "fix": {"old": "a", "new": "b"}},
          open("/app/outputs/finding.json", "w"))
P
want $H 0 "a finding that never says which entry it worked"

# The whole contract, satisfied.
finding "src/users.py" "get_user_by_name" "$INJ" "$UOLD" "$UNEW"
want $H 1 "grounded, proven, threat-modelled, on its queue entry, and the fix flips it"

# A gate that cannot check must not vote, and the queue is now something it
# checks: with no queue mounted there is no assignment to judge against.
mv /app/inputs /app/inputs.off
abstain $H "the hunting queue is not mounted at all"
mv /app/inputs.off /app/inputs

# ---- report gate ----
rm -f $A/report.json $A/finding.json
want $R 0 "nothing at $A/report.json"
all4='{"findings":[{"file":"src/users.py","function":"get_user_by_name"},{"file":"src/orders.py","function":"search_orders_by_status"},{"file":"src/search.py","function":"search_products"},{"file":"src/reports.py","function":"monthly_summary"}]}'
echo "$all4" > $A/report.json
want $R 1 "all four seeded instances and nothing else"
echo '{"findings":[{"file":"src/users.py","function":"get_user_by_name"},{"file":"src/orders.py","function":"search_orders_by_status"},{"file":"src/search.py","function":"search_products"}]}' > $A/report.json
want $R 0 "three of four: recall below 1"

# THE DECOY FORGERY. This target exists to make "flag everything" lose: every
# vulnerable function has a safe sibling doing the analogous read with a bound
# parameter, so a hunter that flags all eight has found nothing.
python3 -c "
import json
gt=json.load(open('/gate/ground_truth.json'))
json.dump({'findings': gt['vulnerable']+gt['safe']}, open('$A/report.json','w'))"
want $R 0 "flag everything: recall 1 but precision 0.5"

# ---- a gate that cannot check must not vote ----
rm -f $A/report.json
echo '{"file":"src/users.py","function":"get_user_by_name","payload":"x"}' > $A/finding.json
mv /gate/src /gate/src.off
abstain $H "the gate's own copy of the target is gone"
mv /gate/src.off /gate/src
mv /gate/ground_truth.json /gate/ground_truth.off
echo "$all4" > $A/report.json
abstain $R "the gate's own ground truth is gone"
mv /gate/ground_truth.off /gate/ground_truth.json


# ---- validate gate: the adversary is scored against the machine ----
I=/app/inputs/hunt-a
V=/gate/validate_check.py
rm -rf /app/inputs && mkdir -p $I
real='{"file":"src/users.py","function":"get_user_by_name","payload":"nobody'"'"' OR '"'"'1'"'"'='"'"'1"}'
dud='{"file":"src/users.py","function":"get_user_by_id","payload":"nobody'"'"' OR '"'"'1'"'"'='"'"'1"}'

echo "$real" > $I/finding.json
echo '{"file":"src/users.py","function":"get_user_by_name","verdict":"confirmed"}' > $A/verdict.json
want $V 1 "the adversary confirms a finding that really proves"
echo '{"file":"src/users.py","function":"get_user_by_name","verdict":"refuted"}' > $A/verdict.json
want $V 0 "the adversary refutes a finding that really proves"
# It may not log findings of its own: a verdict about anything else is refused.
echo '{"file":"src/orders.py","function":"search_orders_by_status","verdict":"confirmed"}' > $A/verdict.json
want $V 0 "the adversary reports a DIFFERENT function than the one it was given"

echo "$dud" > $I/finding.json
echo '{"file":"src/users.py","function":"get_user_by_id","verdict":"refuted"}' > $A/verdict.json
want $V 1 "the adversary refutes a finding that does not prove"
echo '{"file":"src/users.py","function":"get_user_by_id","verdict":"confirmed"}' > $A/verdict.json
want $V 0 "the adversary rubber-stamps a decoy"
rm -f $A/verdict.json

# ---- dedupe gate: a partition, so semantic collapsing is allowed ----
D=/gate/dedupe_check.py
rm -rf /app/inputs && mkdir -p /app/inputs/hunt-a /app/inputs/hunt-b /app/inputs/hunt-c
echo '{"file":"src/users.py","function":"get_user_by_name"}'  > /app/inputs/hunt-a/finding.json
echo '{"file":"src/users.py","function":"get_user_by_name"}'  > /app/inputs/hunt-b/finding.json
echo '{"file":"src/orders.py","function":"search_orders_by_status"}' > /app/inputs/hunt-c/finding.json

U='{"file":"src/users.py","function":"get_user_by_name","absorbed":[{"file":"src/users.py","function":"get_user_by_name"}]}'
O='{"file":"src/orders.py","function":"search_orders_by_status","absorbed":[{"file":"src/orders.py","function":"search_orders_by_status"}]}'
echo "{\"findings\":[$U,$O]}" > $A/deduped.json
want $D 1 "three reports, one duplicate, partitioned into two"

# THE CASE THE OLD GATE GOT BACKWARDS: collapsing two DIFFERENT functions the
# agent judged to be one bug. Cloudflare's dedupe collapses semantically
# equivalent findings; the first version of this gate failed exactly that.
BOTH='{"file":"src/users.py","function":"get_user_by_name","absorbed":[{"file":"src/users.py","function":"get_user_by_name"},{"file":"src/orders.py","function":"search_orders_by_status"}]}'
echo "{\"findings\":[$BOTH]}" > $A/deduped.json
want $D 1 "two distinct functions collapsed into one finding, declared"

echo "{\"findings\":[$U]}" > $A/deduped.json
want $D 0 "a finding absorbed by nobody: work silently dropped"
BAD='{"file":"src/search.py","function":"search_products","absorbed":[{"file":"src/search.py","function":"search_products"}]}'
echo "{\"findings\":[$U,$O,$BAD]}" > $A/deduped.json
want $D 0 "absorbs a finding that was never handed in"
TWICE='{"file":"src/orders.py","function":"search_orders_by_status","absorbed":[{"file":"src/users.py","function":"get_user_by_name"},{"file":"src/orders.py","function":"search_orders_by_status"}]}'
echo "{\"findings\":[$U,$TWICE]}" > $A/deduped.json
want $D 0 "one input absorbed by two keepers: not a partition"
echo "{\"findings\":[{\"file\":\"src/users.py\",\"function\":\"get_user_by_name\"}]}" > $A/deduped.json
want $D 0 "a kept finding that declares no absorbed list"
rm -f $A/deduped.json

# ---- trace gate: every hop is a real call edge ----
T=/gate/trace_check.py
rm -rf /app/inputs && mkdir -p /app/inputs/dedupe
echo '{"findings":[{"file":"src/users.py","function":"get_user_by_name"}]}' > /app/inputs/dedupe/deduped.json
echo '{"traces":[{"function":"get_user_by_name","path":["handle_user_lookup","get_user_by_name"]}]}' > $A/trace.json
want $T 1 "a path whose every hop is a real call edge"
# THE FORGERY: a plausible path through functions that never call each other.
echo '{"traces":[{"function":"get_user_by_name","path":["handle_catalog","get_user_by_name"]}]}' > $A/trace.json
want $T 0 "a plausible path whose hop is not a call this source makes"
echo '{"traces":[{"function":"get_user_by_name","path":["handle_user_lookup","nonexistent_fn","get_user_by_name"]}]}' > $A/trace.json
want $T 0 "a path through a function that does not exist"
echo '{"traces":[]}' > $A/trace.json
want $T 0 "no reachability path for a finding that was handed in"
rm -rf /app/inputs $A/trace.json

rm -f $A/finding.json $A/report.json
echo "SELFTEST OK"
