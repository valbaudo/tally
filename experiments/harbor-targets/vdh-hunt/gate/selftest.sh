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

# ---- hunt gate ----
rm -f $A/finding.json
want $H 0 "nothing at $A/finding.json"
echo 'not json' > $A/finding.json
want $H 0 "the finding is not JSON"
echo '{}' > $A/finding.json
want $H 0 "the finding names no function"
echo '{"file":"src/nope.py","function":"x","payload":"y"}' > $A/finding.json
want $H 0 "the cited module is not in this target"

# THE TWO FORGERIES, and they are why this gate runs the function instead of
# checking a list. The source is IN the agent's image, so a function name is
# free to guess; what is not free is a payload that actually changes a query.
echo '{"file":"src/users.py","function":"get_user_by_name","payload":"alice"}' > $A/finding.json
want $H 0 "a real vulnerable function named with a payload that exploits nothing"
echo '{"file":"src/users.py","function":"get_user_by_id","payload":"nobody'"'"' OR '"'"'1'"'"'='"'"'1"}' > $A/finding.json
want $H 0 "a decoy named with a real injection payload"

# The four seeded instances, each proven by running it.
for c in 'src/users.py:get_user_by_name:nobody'"'"' OR '"'"'1'"'"'='"'"'1' \
         'src/orders.py:search_orders_by_status:x'"'"' OR '"'"'1'"'"'='"'"'1' \
         'src/search.py:search_products:%'"'"' OR '"'"'1'"'"'='"'"'1' \
         'src/reports.py:monthly_summary:x'"'"' OR '"'"'1'"'"'='"'"'1'; do
  f=${c%%:*}; rest=${c#*:}; n=${rest%%:*}; p=${rest#*:}
  python3 -c "import json,sys;json.dump({'file':sys.argv[1],'function':sys.argv[2],'payload':sys.argv[3]},open('$A/finding.json','w'))" "$f" "$n" "$p"
  want $H 1 "the seeded instance $f:$n"
done

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
I=/app/inputs/0
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

# ---- dedupe gate: collapse without inventing or dropping ----
D=/gate/dedupe_check.py
rm -rf /app/inputs && mkdir -p /app/inputs/0 /app/inputs/1 /app/inputs/2
echo '{"file":"src/users.py","function":"get_user_by_name"}'  > /app/inputs/0/finding.json
echo '{"file":"src/users.py","function":"get_user_by_name"}'  > /app/inputs/1/finding.json
echo '{"file":"src/orders.py","function":"search_orders_by_status"}' > /app/inputs/2/finding.json
echo '{"findings":[{"file":"src/users.py","function":"get_user_by_name"},{"file":"src/orders.py","function":"search_orders_by_status"}]}' > $A/deduped.json
want $D 1 "three findings, one duplicate, collapsed to two"
echo '{"findings":[{"file":"src/users.py","function":"get_user_by_name"},{"file":"src/users.py","function":"get_user_by_name"}]}' > $A/deduped.json
want $D 0 "the output still contains a duplicate"
echo '{"findings":[{"file":"src/users.py","function":"get_user_by_name"}]}' > $A/deduped.json
want $D 0 "dedupe dropped a distinct finding instead of collapsing one"
echo '{"findings":[{"file":"src/users.py","function":"get_user_by_name"},{"file":"src/orders.py","function":"search_orders_by_status"},{"file":"src/search.py","function":"search_products"}]}' > $A/deduped.json
want $D 0 "dedupe invented a finding that was never handed in"
rm -f $A/deduped.json

# ---- trace gate: every hop is a real call edge ----
T=/gate/trace_check.py
rm -rf /app/inputs && mkdir -p /app/inputs/0
echo '{"findings":[{"file":"src/users.py","function":"get_user_by_name"}]}' > /app/inputs/0/deduped.json
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
