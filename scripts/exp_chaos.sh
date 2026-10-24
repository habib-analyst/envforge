#!/bin/bash
# E4: chaos — kill -9 the provisioner at every phase, then tear down
# from a fresh process and run the independent leak audit.
set -u
source "$(dirname "$0")/common.sh"
PHASEFILE="$EXP/phase.txt"
RESULTS="$EXP/chaos_cases.tsv"
echo -e "case\ttarget_phase\tkilled_at\tleaks_after_fresh_teardown" > "$RESULTS"

kill_at_phase() { # env_name phase [--hold]
  local env="$1" phase="$2" hold="${3:-}"
  rm -f "$PHASEFILE"
  ENVFORGE_PHASE_FILE="$PHASEFILE" ENVFORGE_SLOW_PHASE_MS=500 \
    "$EF" up --name "$env" --spec "$ROOT/examples/stack.json" $hold > "$EXP/chaos_up_$env.log" 2>&1 &
  local pid=$!
  local seen=""
  for i in $(seq 1 3000); do
    seen="$(cat "$PHASEFILE" 2>/dev/null | tr -d '\n')"
    [ "$seen" = "$phase" ] && break
    kill -0 $pid 2>/dev/null || break
    sleep 0.01
  done
  kill -9 $pid 2>/dev/null; wait $pid 2>/dev/null
  echo "$seen"
}

audit_clean_for() { # env -> leak lines (empty = clean)
  "$EF" down --name "$1" >> "$EXP/chaos_down.log" 2>&1
  "$EF" audit --name "$1" 2>&1 | grep '^LEAK:' || true
}

for phase in alloc dirs start:redis start:api start:worker start:web health smoke; do
  env="pr-ch-${phase//:/-}"
  seen="$(kill_at_phase "$env" "$phase")"
  leaks="$(audit_clean_for "$env")"
  n=$(printf '%s' "$leaks" | grep -c . || true)
  echo -e "kill-during-up\t$phase\t$seen\t$n" >> "$RESULTS"
  echo "case kill-during-up@$phase (landed: $seen) leaks=$n"
  [ -n "$leaks" ] && echo "$leaks"
done

# Idle: provisioner alive after ready (up --hold), killed, fresh teardown.
env="pr-ch-idle"
seen="$(kill_at_phase "$env" "ready" "--hold")"
leaks="$(audit_clean_for "$env")"
n=$(printf '%s' "$leaks" | grep -c . || true)
echo -e "kill-at-idle\tready\t$seen\t$n" >> "$RESULTS"
echo "case kill-at-idle (landed: $seen) leaks=$n"
[ -n "$leaks" ] && echo "$leaks"

# Mid-teardown: clean up, then kill -9 the teardown at down:wait,
# re-run teardown from a fresh process, audit.
env="pr-ch-downkill"
"$EF" up --name "$env" --spec "$ROOT/examples/stack.json" > /dev/null 2>&1
rm -f "$PHASEFILE"
ENVFORGE_PHASE_FILE="$PHASEFILE" ENVFORGE_SLOW_PHASE_MS=600 \
  "$EF" down --name "$env" > "$EXP/chaos_downkill.log" 2>&1 &
dpid=$!
seen=""
for i in $(seq 1 3000); do
  seen="$(cat "$PHASEFILE" 2>/dev/null | tr -d '\n')"
  [ "$seen" = "down:wait" ] && break
  kill -0 $dpid 2>/dev/null || break
  sleep 0.01
done
kill -9 $dpid 2>/dev/null; wait $dpid 2>/dev/null
leaks="$(audit_clean_for "$env")"
n=$(printf '%s' "$leaks" | grep -c . || true)
echo -e "kill-during-teardown\tdown:wait\t$seen\t$n" >> "$RESULTS"
echo "case kill-during-teardown (landed: $seen) leaks=$n"
[ -n "$leaks" ] && echo "$leaks"

# Global audit: nothing of any env may remain.
GLOBAL="$("$EF" audit 2>&1)"
echo "global audit: $GLOBAL"
echo "CHAOS_DONE"
