# envforge

**Ephemeral per-PR environments on one local host — with a teardown
guarantee that is *proven by chaos*, not promised.**

Every ephemeral-environment tool (Qovery, Uffizzi, Heroku Review Apps,
Terraform + a reaper cron) treats teardown as a happy-path step in the
same process that provisioned. If that process dies — CI runner killed,
laptop closed, `kill -9` — the environment leaks, and a TTL reaper mops
it up hours later, *maybe*. envforge is built around the opposite
assumption: **the provisioner will die at the worst possible moment**,
and teardown must still be exact. That claim is not asserted; it is
measured (see [PAPER.md](PAPER.md)):

- the provisioner is `kill -9`'d at **every** phase boundary —
  mid-provision, mid-smoke, idle, and mid-teardown itself — and an
  independent leak audit (which does *not* trust envforge's own
  bookkeeping) finds **zero** orphaned processes, ports, data
  directories, or database state (**10/10 chaos cases, 0 leaks**,
  measured in [PAPER.md](PAPER.md));
- two environments built from the *same* spec run concurrently and
  cross-reads are attempted: **0 violations**;
- drift between the spec and the live environment is a first-class
  command, checked by process identity (PID + start time, so PID reuse
  and port squatters cannot fool it), listening ports, HTTP config
  read-back, rendered files, and the spec's hash.

Local-first and $0: no cloud, no Kubernetes, no Docker. An environment
is plain processes on one host — a per-env Redis, a per-env PostgreSQL,
and your app services — allocated a private port block and data
directory from a JSON spec.

## Quick start

```sh
go build -o bin/envforge ./cmd/envforge
go build -o bin/appsvc  ./examples/appsvc   # demo app service

export ENVFORGE_HOME=~/.envforge            # state root (keep it SHORT:
                                            # Postgres unix-socket paths
                                            # are capped at 107 chars)
export ENVFORGE_APPSVC=$PWD/bin/appsvc

bin/envforge up     --name pr-123 --spec examples/stack.json
bin/envforge list
bin/envforge verify --name pr-123           # drift check (exit 2 on drift)
bin/envforge down   --name pr-123           # exact teardown, any process
bin/envforge audit                          # independent leak audit
```

`examples/stack.json` is a complete stack: one Redis + three app
services (`api`, `worker`, `web`) with cross-service smoke checks.
`examples/stackpg.json` adds a per-env PostgreSQL.
`examples/stack1.json` is a single service.

Measured on this repo's 2-core VM (details and method in PAPER.md):

| stack | p50 provision | p95 provision |
|---|---|---|
| 1 service | 113 ms | 134 ms |
| Redis + 3 services | 201 ms | 229 ms |
| Redis + PostgreSQL + 3 services | 12.8 s | 14.1 s |

(PostgreSQL is dominated by `initdb`'s fsync time on shared storage;
the < 60 s target for a full stack holds across all measured stacks.)

## The spec

```json
{
  "name": "pr-123",
  "infra": {"redis": true, "postgres": true},
  "services": [
    {"name": "api", "command": ["{{.Appsvc}}"],
     "env": {"FEATURE_FLAG": "on"}, "health": "/health", "dataDir": true},
    {"name": "web", "command": ["{{.Appsvc}}"],
     "env": {"FEATURE_FLAG": "on"}, "health": "/health"}
  ],
  "smoke": [{"service": "web", "path": "/call?svc=api", "expect": 200}]
}
```

Commands and env values are Go templates rendered per env:
`{{.Env}}`, `{{.Dir}}`, `{{.Port}}` (the service's own port),
`{{.PortFor "api"}}`, `{{.RedisPort}}`, `{{.PGPort}}`, `{{.Appsvc}}`.
Ports come from a per-env block (base 20000, 100 ports per env).
Services also receive injected env vars: `PORT`, `REDIS_ADDR`,
`DATABASE_URL`, and `EF_<SERVICE>_URL` for every sibling service.

## How the guarantee works

1. **Registry first.** The environment record — with its port block —
   is written to a locked, atomically-renamed JSON registry *before*
   anything is created, and every spawned process is recorded
   *immediately* after it starts. A `kill -9` therefore never loses
   track of more than a just-spawned process, and:
2. **Marker second.** Every child process carries `ENVFORGE_ENV` /
   `ENVFORGE_SVC` in its environment. Teardown and audit scan `/proc`
   for these markers and adopt *any* process belonging to the env —
   including one the provisioner died a microsecond before recording.
3. **Teardown is stateless and idempotent.** `down` runs entirely from
   the registry + marker scan, from a fresh process, and can be re-run
   after being killed itself. Process identity is PID + `/proc` start
   time, so a recycled PID is never signalled by mistake.
4. **Audit trusts nothing.** `audit` cross-checks three independent
   sources — `/proc` markers, the kernel listen table, and the
   filesystem — against the set of live envs. Run it in CI after every
   teardown; exit code 2 means something leaked.

PostgreSQL per env runs under `unshare --map-user=65534`: this host's
shell is uid 0, Postgres refuses root, and `nobody` is the one uid that
even a minimal container `/etc/passwd` always resolves for `initdb`.

## Commands

| command | what it does |
|---|---|
| `up --name N --spec F [--hold]` | provision (alloc → dirs → start → health → smoke → ready) |
| `down --name N` | kill process trees, remove dirs, unregister (idempotent) |
| `list` | live environments, states, phases, ports |
| `verify --name N` | drift vs spec: hash, processes, ports, `/config` read-back, rendered files |
| `audit [--name N]` | leak audit: orphan processes, ports, dirs, registry residue |

Chaos and experiment harnesses live in `scripts/`
(`exp_timing.sh`, `exp_isolation.sh`, `exp_drift.sh`, `exp_chaos.sh`);
raw outputs in `experiments/`, merged results in `results.json`.

## Honest limits

- **One host, processes not sandboxes.** Isolation is per-env
  processes, ports, data dirs, and database instances — not VMs or
  containers. A hostile service can still read another env's files;
  this is a dev/CI tool, not a security boundary.
- **Shared CPU is real.** Under a 10 s write-hammer on env A, env B's
  health p95 rose from 0.81 ms to 7.57 ms on the 2-core test VM.
  Isolation of *data* is exact; isolation of *performance* is not.
- **Health checks trust ports.** A squatter already listening on an
  assigned port makes provision fail loudly (pre-flight check), but a
  squatter that binds *after* the check and *before* the service can
  still confuse a health probe; process-identity verification in
  `verify` is the backstop.
- **initdb variance.** PostgreSQL provisioning time varies widely
  (4.7–14.1 s measured) with the host's fsync latency.

## Layout

```
cmd/envforge/        CLI
internal/spec/       spec format, hashing, template rendering
internal/registry/   locked, atomic JSON state store (+ tombstones)
internal/procutil/   /proc discovery: markers, start times, listen table
internal/engine/     provision, teardown, verify, audit
examples/            demo app service + three specs
scripts/             experiment + chaos harnesses
```
