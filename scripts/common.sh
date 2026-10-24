# Shared setup for envforge experiment scripts.
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export ENVFORGE_HOME="${ENVFORGE_HOME:-$HOME/workspace/efhome}"
export ENVFORGE_APPSVC="$ROOT/bin/appsvc"
EF="$ROOT/bin/envforge"
REDIS_CLI="$HOME/workspace/tools/redis-src/redis-7.4.2/src/redis-cli"
EXP="$ROOT/experiments"
mkdir -p "$EXP"
