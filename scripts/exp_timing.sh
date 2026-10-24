#!/bin/bash
# E1: provision-time distribution per stack size.
set -u
source "$(dirname "$0")/common.sh"
N="${N:-8}"
OUT="$EXP/timing.csv"
echo "config,iter,duration_ms" > "$OUT"
run_config() {
  local cfg="$1" spec="$2"
  for i in $(seq 1 "$N"); do
    local name="pr-t-$cfg-$i"
    local json
    json="$("$EF" up --name "$name" --spec "$spec" 2>/dev/null | tail -1)"
    local ms
    ms="$(printf '%s' "$json" | python3 -c 'import sys,json; print(json.load(sys.stdin)["duration_ms"])' 2>/dev/null || echo FAIL)"
    echo "$cfg,$i,$ms" >> "$OUT"
    "$EF" down --name "$name" > /dev/null 2>&1
  done
}
run_config stack1 "$ROOT/examples/stack1.json"
run_config stack  "$ROOT/examples/stack.json"
run_config stackpg "$ROOT/examples/stackpg.json"
python3 - "$OUT" "$EXP/timing.json" << 'PYEOF'
import csv, json, sys, statistics
rows = {}
for r in csv.DictReader(open(sys.argv[1])):
    if r["duration_ms"] == "FAIL":
        continue
    rows.setdefault(r["config"], []).append(int(r["duration_ms"]))
def pct(v, p):
    v = sorted(v)
    k = max(0, min(len(v)-1, int(round((p/100.0)*(len(v)-1)))))
    return v[k]
out = {}
for cfg, v in rows.items():
    out[cfg] = {"n": len(v), "min_ms": min(v), "p50_ms": pct(v,50),
                "p95_ms": pct(v,95), "max_ms": max(v),
                "mean_ms": round(statistics.mean(v),1), "samples_ms": v}
json.dump(out, open(sys.argv[2], "w"), indent=2)
print(json.dumps(out, indent=2))
PYEOF
