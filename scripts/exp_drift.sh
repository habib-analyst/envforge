#!/bin/bash
# E3: drift detection. Baseline (clean), then three induced drifts +
# one squatter, each verified separately.
set -u
source "$(dirname "$0")/common.sh"
ENV=pr-drift
SPEC="$ROOT/examples/stack.json"
declare -A FOUND
"$EF" up --name $ENV --spec "$SPEC" > /dev/null 2>&1

# Baseline: no drift expected.
BASE="$("$EF" verify --name $ENV 2>&1)"; BASE_RC=$?
echo "baseline rc=$BASE_RC: $BASE"

# (a) Spec file edited after provision -> hash drift.
cp "$SPEC" "$EXP/spec-backup.json"
python3 - << 'PYEOF'
import re
p = "examples/stack.json"
s = open(p).read()
s = s.replace('"FEATURE_FLAG": "on", "TIER": "api"', '"FEATURE_FLAG": "off", "TIER": "api"', 1)
open(p, "w").write(s)
PYEOF
A="$("$EF" verify --name $ENV 2>&1)"
echo "case (a) spec edit:"; echo "$A"
cp "$EXP/spec-backup.json" "$SPEC"

# (b) Rendered env file on disk edited -> rendered-config drift.
ENVF="$ENVFORGE_HOME/envs/$ENV/api.env"
cp "$ENVF" "$EXP/api.env.bak"
sed -i 's/^FEATURE_FLAG=on$/FEATURE_FLAG=off/' "$ENVF"
B="$("$EF" verify --name $ENV 2>&1)"
echo "case (b) rendered file edit:"; echo "$B"
cp "$EXP/api.env.bak" "$ENVF"

# (c) Kill worker's process group -> process drift for worker only.
WPGID=$(python3 -c "import json; d=json.load(open('$ENVFORGE_HOME/registry.json')); print([p['pgid'] for p in d['envs']['$ENV']['procs'] if p['service']=='worker'][0])")
WPORT=$(python3 -c "import json; d=json.load(open('$ENVFORGE_HOME/registry.json')); print(d['envs']['$ENV']['ports']['worker'])")
kill -9 -"$WPGID"
C=""
for i in $(seq 1 50); do
  C="$("$EF" verify --name $ENV 2>&1)"
  case "$C" in *"worker: process not running"*) break;; esac
  sleep 0.1
done
echo "case (c) worker killed:"; echo "$C"

# (d) Squatter on worker's port with look-alike config: identity check
# (pid+start time) must still flag worker; port check alone would pass.
PORT=$WPORT ENVFORGE_SVC=worker FEATURE_FLAG=off TIER=worker EF_NO_CHILD=1 "$ENVFORGE_APPSVC" > /dev/null 2>&1 &
SQ=$!
sleep 0.5
D="$("$EF" verify --name $ENV 2>&1)"
echo "case (d) squatter on worker port $WPORT:"; echo "$D"
kill -9 $SQ 2>/dev/null

"$EF" down --name $ENV > /dev/null 2>&1
AUD="$("$EF" audit --name $ENV 2>&1)"
echo "post-teardown audit: $AUD"
python3 - "$EXP" "$BASE_RC" "$A" "$B" "$C" "$D" << 'PYEOF'
import json, sys
exp, base_rc, a, b, c, d = sys.argv[1:7]
lines = lambda s: [l.replace("DRIFT: ", "") for l in s.splitlines() if l.startswith("DRIFT:")]
out = {
  "baseline": {"exit_code": int(base_rc), "drift_items": [] if int(base_rc) == 0 else ["<see log>"]},
  "case_a_spec_file_edited": lines(a),
  "case_b_rendered_env_file_edited": lines(b),
  "case_c_worker_process_killed": lines(c),
  "case_d_squatter_on_worker_port": lines(d),
}
json.dump(out, open(exp + "/drift.json", "w"), indent=2)
print(json.dumps(out, indent=2))
PYEOF
