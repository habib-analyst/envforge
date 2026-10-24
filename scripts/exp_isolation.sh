#!/bin/bash
# E2: two envs from the SAME spec, concurrently. Cross-read attempts +
# interference measurement (hammer A, measure B's /health latency).
set -u
source "$(dirname "$0")/common.sh"
A=pr-iso-a; B=pr-iso-b
"$EF" up --name $A --spec "$ROOT/examples/stack.json" > "$EXP/iso_a_up.json" 2>&1 &
"$EF" up --name $B --spec "$ROOT/examples/stack.json" > "$EXP/iso_b_up.json" 2>&1 &
wait
port() { python3 -c "import json,sys; d=json.load(open('$ENVFORGE_HOME/registry.json')); print(d['envs']['$1']['ports']['$2'])"; }
ARA=$(port $A redis); AAPI=$(port $A api)
BRD=$(port $B redis); BAPI=$(port $B api); BWEB=$(port $B web)
echo "A redis=$ARA api=$AAPI | B redis=$BRD api=$BAPI web=$BWEB"

# Distinct data in each env's Redis + app tier.
"$REDIS_CLI" -p $ARA SET secret-a "A-ONLY" > /dev/null
"$REDIS_CLI" -p $BRD SET secret-b "B-ONLY" > /dev/null
curl -s -X POST "http://127.0.0.1:$AAPI/kv" -d '{"K":"shared-key","V":"from-A"}' > /dev/null
curl -s -X POST "http://127.0.0.1:$BAPI/kv" -d '{"K":"shared-key","V":"from-B"}' > /dev/null

# Cross-read attempts (want: all fail to see the other env's data).
X1=$("$REDIS_CLI" -p $BRD GET secret-a)          # B reading A's redis key
X2=$("$REDIS_CLI" -p $ARA GET secret-b)          # A reading B's redis key
X3=$(curl -s "http://127.0.0.1:$BAPI/kv?k=shared-key" | python3 -c 'import sys,json; print(json.load(sys.stdin)["value"])')
X4=$(curl -s "http://127.0.0.1:$AAPI/kv?k=shared-key" | python3 -c 'import sys,json; print(json.load(sys.stdin)["value"])')
CROSS=0
[ -n "$X1" ] && CROSS=$((CROSS+1))
[ -n "$X2" ] && CROSS=$((CROSS+1))
[ "$X3" = "from-A" ] && CROSS=$((CROSS+1))
[ "$X4" = "from-B" ] && CROSS=$((CROSS+1))
echo "cross-read violations: $CROSS (A sees '$X4' for shared-key, B sees '$X3')"

sample_latency() { # port, count -> space-separated ms values
  for i in $(seq 1 "$2"); do
    curl -s -o /dev/null -w '%{time_total}\n' "http://127.0.0.1:$1/health"
  done
}
sample_latency $BWEB 50 > "$EXP/iso_base.txt"
# Hammer env A for ~10 s while sampling B.
( end=$((SECONDS+10)); n=0
  while [ $SECONDS -lt $end ]; do
    curl -s -X POST "http://127.0.0.1:$AAPI/kv" -d "{\"K\":\"hammer\",\"V\":\"$n\"}" -o /dev/null
    n=$((n+1))
  done; echo $n > "$EXP/iso_hammer_count.txt" ) &
HAMMER=$!
sample_latency $BWEB 50 > "$EXP/iso_loaded.txt"
wait $HAMMER
"$EF" down --name $A > /dev/null 2>&1
"$EF" down --name $B > /dev/null 2>&1
python3 - "$EXP" "$CROSS" << 'PYEOF'
import json, sys
exp, cross = sys.argv[1], int(sys.argv[2])
def stats(path):
    v = sorted(float(l) * 1000 for l in open(path) if l.strip())
    p = lambda q: v[min(len(v)-1, int(round(q*(len(v)-1))))]
    return {"n": len(v), "p50_ms": round(p(0.50), 2), "p95_ms": round(p(0.95), 2), "max_ms": round(v[-1], 2)}
out = {"cross_read_violations": cross,
       "env_b_health_latency_idle": stats(exp + "/iso_base.txt"),
       "env_b_health_latency_while_a_hammered": stats(exp + "/iso_loaded.txt"),
       "hammer_writes_to_a": int(open(exp + "/iso_hammer_count.txt").read())}
json.dump(out, open(exp + "/isolation.json", "w"), indent=2)
print(json.dumps(out, indent=2))
PYEOF
