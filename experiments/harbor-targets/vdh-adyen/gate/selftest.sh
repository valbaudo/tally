#!/bin/bash
# Build-time proof that this image honours the contract, run as the last layer.
#
# CEILING, stated because it matters: the live half CANNOT be exercised here.
# A reward of 1 needs the deployment to serve the canary, and the build has no
# network. So every case below is one the gate refuses BEFORE any request is
# made, which is exactly why they are provable offline. The live half is proven
# once, by a recorded run, not by every build.
set -uo pipefail

# The cases run against an ARMED copy, because an unarmed gate refuses to vote
# at all -- which is itself the last case here, and the important one.
ARMED=/tmp/armed.py
sed 's/^CANARY = .*/CANARY = "c0ffee00c0ffee00c0ffee00c0ffee00"/' /gate/check.py > $ARMED

want() { python3 $ARMED >/dev/null 2>&1
         r=$(python3 -c 'import json;print(json.load(open("/logs/verifier/reward.json"))["reward"])' 2>/dev/null)
         raw=$(cat /logs/verifier/reward.json 2>/dev/null); rm -rf /logs
         [ "$r" = "$1" ] || { echo "SELFTEST FAILED: $2 -> ${raw:-no reward.json}"; exit 1; }; }
# A gate that cannot check must not vote: no reward.json at all, which classify
# reads as infra_error rather than as a rejection of an agent nobody checked.
abstain() { python3 ${2:-$ARMED} >/dev/null 2>&1; r=$(ls /logs/verifier/reward.json 2>/dev/null); rm -rf /logs
            [ -z "$r" ] || { echo "SELFTEST FAILED: $1 -> wrote reward.json, expected none"; exit 1; }; }
mkdir -p /app/outputs

want 0 "nothing at /app/outputs/pov.json"

echo 'not json' > /app/outputs/pov.json
want 0 "pov.json is not JSON"

cat > /app/outputs/pov.json <<'P'
{"class":"x","file":"src/Nope.php","line":1,"evidence":"whatever","request":{"path":"/a"}}
P
want 0 "citation names a file that is not in the plugin"

# A real citation, taken from the gate's own copy, for the cases below.
F=$(cd /gate/repo && grep -rn 'function ' --include='*.php' src | head -1)
FILE=$(echo "$F" | cut -d: -f1); LINE=$(echo "$F" | cut -d: -f2)
EV=$(sed -n "${LINE}p" "/gate/repo/$FILE" | sed 's/^[[:space:]]*//;s/"/\\"/g')
pov() { python3 - "$FILE" "$LINE" "$EV" "$1" > /app/outputs/pov.json <<'P'
import json,sys
f,l,ev,req=sys.argv[1],int(sys.argv[2]),sys.argv[3],json.loads(sys.argv[4])
print(json.dumps({"class":"x","file":f,"line":l,"evidence":ev,"request":req}))
P
}

pov '{"path":"/x","query":{"q":"c0ffee00c0ffee00c0ffee00c0ffee00"}}'
want 0 "the canary sits in the request (a reflected value proves nothing)"

pov '{"path":"https://elsewhere.example/x"}'
want 0 "the request is not a site-relative path"

pov '{"path":"/x","headers":{"Host":"elsewhere.example"}}'
want 0 "the request tries to set Host"

# THE FORGERY CASE. Every previous version of this gate let the finding supply
# both sides of a comparison, and two artifacts that found nothing scored 1:
# "/" (200) against "/api/order/<junk>" (401), and the same pair with marker
# "Shopware". Neither is expressible now -- there is no control and no marker
# to supply, and this proves the shape is gone rather than merely unused.
cat > /app/outputs/pov.json <<'P'
{"class":"x","file":"src/Nope.php","line":1,"evidence":"whatever",
 "exploit":{"path":"/"},"control":{"path":"/api/order/junk"},"marker":"Shopware","proof":"authz"}
P
want 0 "the old exploit/control/marker forgery, which no longer even parses as a finding"

# The gate's own baked source gone: it must die writing nothing.
mv /gate/repo /gate/repo.off
abstain "the gate's own plugin source is gone"
mv /gate/repo.off /gate/repo

# THE FAIL-CLOSED CASE, and the reason the canary design is safe to ship
# unarmed: with no canary planted there is no oracle, so the gate must write
# nothing. If it voted 0 instead it would report "the plugin is sound" on the
# strength of an oracle nobody armed -- a fabricated rejection, which is the one
# thing the contract forbids. /gate/check.py is the SHIPPED file, still UNARMED.
pov '{"path":"/"}'
abstain "no canary is planted, so the gate has no oracle" /gate/check.py

rm -f /app/outputs/pov.json $ARMED
echo "SELFTEST OK (offline mechanics; the live half is proven by a recorded run)"

# Two callers, and they need different answers.
#
# The BUILD calls this as `selftest.sh build`, and an unarmed image is a fine
# thing to build: the repo holds no canary, so that is what a clean checkout
# produces, and every case above proved the mechanics without one.
#
# dawn calls it with no argument, from proveGate, BEFORE it dispatches an agent
# (harbor.go). There an unarmed gate must refuse: it has no oracle, so it would
# abstain after the agent had already run, and the whole point of proving a gate
# first is that a gate which cannot produce a verdict costs nothing to discover.
if [ "${1:-}" != "build" ] && grep -q '^CANARY = "UNARMED"' /gate/check.py; then
  echo "REFUSING: no canary is planted, so this gate has no oracle (see gate/README.md)"
  exit 1
fi
