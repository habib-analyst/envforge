# envforge: a local ephemeral-environment provisioner whose teardown guarantee is measured, not promised

**Date:** 2026-10-23 · **Host:** 2-core Linux VM, Go 1.23.4, Redis 7.4.2,
PostgreSQL 16.4 · **Raw data:** `results.json`, `experiments/`

## 1. The claim

Ephemeral-environment tools treat teardown as a happy-path step executed
by the same process that provisioned. When that process dies — the normal
case in CI, where runners are killed, and on laptops, where lids close —
the environment leaks, and the industry's answer is a TTL reaper that
mops up later, most of the time (a widely read GKE preview-environment
tutorial concedes that even ~95%-effective cleanup still leaks, and
budgets a scheduled reaper for the rest). Terraform's destroy has the
dual problem: it deletes what its *state file* says, and a mid-apply
kill is precisely how state files become stale or corrupt.

envforge inverts the design. Teardown is a *stateless, idempotent*
operation over durable state, runnable by any fresh process, and the
system is engineered so that the provisioner's death at any instant
loses at most one just-spawned, not-yet-recorded process — which a
second, registry-independent discovery channel (`/proc` environment
markers) recovers. This paper reports the measurements behind the four
guarantees: provision speed, isolation, drift detection, and
teardown completeness under `kill -9` chaos.

## 2. Method

- **Stacks.** `stack1`: 1 app service. `stack`: per-env Redis + 3 app
  services (api, worker, web) with cross-service smoke checks.
  `stackpg`: Redis + PostgreSQL + 3 services. The app service is a real
  (if small) HTTP service with a Redis-backed KV store, a `/config`
  read-back endpoint, cross-service calls, and a spawned child process
  (so process *trees*, not just leaders, are exercised).
- **Timing.** 8 provisions per stack, duration measured inside the
  provisioner (spec load → ready), each env torn down before the next.
- **Isolation.** Two envs from the *identical* spec, concurrently.
  Distinct secrets per env; cross-reads attempted over both Redis and
  the app tier. Interference: 50 `/health` samples of env B idle, then
  50 more while env A absorbs a 10 s write hammer.
- **Drift.** One clean baseline + four induced faults, verified
  separately on one live env.
- **Chaos.** The provisioner (and, in one case, the teardown) is
  `kill -9`'d at each phase boundary. Phases are made reliably
  hittable by a documented slow-phase injection (500–600 ms per phase)
  and a phase file the killer polls at 10 ms. After each kill, a
  *fresh* `envforge down` runs, then the leak audit. The audit does
  not trust the registry: it cross-checks `/proc` env markers, the
  kernel listen table over the managed port range, and the envs
  directory against the set of live envs.

## 3. Results

### 3.1 Provision time (n=8 per stack)

| stack | min | p50 | p95 | max | mean |
|---|---|---|---|---|---|
| 1 service | 64 ms | 113 ms | 134 ms | 134 ms | 94 ms |
| Redis + 3 services | 124 ms | 201 ms | 229 ms | 229 ms | 179 ms |
| Redis + Postgres + 3 services | 4.73 s | 12.77 s | 14.08 s | 14.08 s | 10.82 s |

All stacks are far under the 60 s full-stack target. The Postgres
stack's cost is almost entirely `initdb`, and its variance (4.7–14.1 s)
tracks this VM's fsync latency on shared storage, not envforge logic:
the non-Postgres stacks are two orders of magnitude faster and tight.

### 3.2 Isolation (two envs, same spec, concurrent)

- **Cross-read violations: 0.** Env B reading env A's Redis key: nil.
  Env A reading env B's: nil. Same key (`shared-key`) written through
  each env's app tier returns each env's *own* value on read-back —
  same service names, same key, disjoint data, because each env has
  its own Redis process, port block, and data directory.
- **Interference (honest):** env B `/health` latency, idle: p50 0.44 ms,
  p95 0.81 ms, max 1.01 ms. While env A took 1,078 KV writes in 10 s:
  p50 0.68 ms, **p95 7.57 ms, max 11.24 ms**. Data isolation is exact;
  performance isolation on a shared 2-core host is not, and the tail
  shows it. This is a limit of process-level isolation, stated rather
  than hidden.

### 3.3 Drift detection (one baseline + four induced faults)

- Baseline: `verify` exit 0, zero items.
- **Spec file edited** (one service's `FEATURE_FLAG` flipped): flagged
  three consistent items — spec sha256 mismatch; live `/config`
  read-back `FEATURE_FLAG = "on"` vs edited spec `"off"`; rendered env
  file differs from the fresh render of the edited spec.
- **Rendered env file edited on disk**: flagged exactly one item —
  that service's rendered file differs from spec render. (Live config
  still matched the spec, correctly not flagged.)
- **Worker process group killed**: flagged exactly one item —
  `worker: process not running`. Api, web, and redis were not flagged.
- **Squatter on the dead worker's port** (a look-alike process with
  matching config, listening on the right port): still flagged exactly
  `worker: process not running`. A port-only check would have passed;
  the identity check (recorded PID + `/proc` start time) is what
  catches it. PID-reuse cannot produce a false "alive" for the same
  reason.

### 3.4 Chaos: teardown completeness (10 cases)

The killer landed on the intended phase in **10/10** cases. After a
fresh-process teardown, the audit found **0 leaks in every case**:

| case | killed at | leaks |
|---|---|---|
| kill during up | alloc | 0 |
| kill during up | dirs | 0 |
| kill during up | start:redis | 0 |
| kill during up | start:api | 0 |
| kill during up | start:worker | 0 |
| kill during up | start:web | 0 |
| kill during up | health | 0 |
| kill during up | smoke | 0 |
| kill at idle (provisioner holding) | ready | 0 |
| kill during teardown itself | down:wait | 0 |

A final global audit over all envs also reported clean: no orphaned
processes, no listening ports in the managed range, no leftover
directories, no registry residue.

Two failure classes found during development are worth recording
because the fixes are part of the design, not scaffolding:

1. **Zombie blindness.** A killed child that its parent hasn't reaped
   keeps a `/proc` entry with the original start time, so a naive
   "pid + starttime" aliveness check reports a dead service as alive
   (while its sockets are already closed — the exact signature the
   first integration test produced). Aliveness now also requires a
   non-zombie state.
2. **Squatter impersonation.** A leaked env from a failed test run
   held the next run's ports and *answered its health checks*, making
   a provision look successful against the wrong processes.
   Provision now pre-flights that every assigned port is free and
   fails loudly otherwise; `verify`'s identity check remains the
   backstop (§3.3, squatter case).

A platform note: per-env Postgres requires running `initdb`/`postgres`
in a user namespace (host shell is uid 0; Postgres refuses root).
The namespace maps to uid 65534 (`nobody`) rather than an arbitrary
uid, because `initdb` resolves the effective uid in `/etc/passwd` and
this VM's minimal container passwd file has no uid-1000 entry — a
failure observed in practice on one exec instance after identical
commands had succeeded on another. `nobody` is the uid that minimal
passwd files always carry.

## 4. Limits

- **One host, process-level isolation.** No VMs, containers, or
  network namespaces: data isolation is exact, performance isolation
  is best-effort (§3.2), and this is a dev/CI tool, not a security
  boundary against hostile code.
- **Chaos coverage is phase boundaries**, not arbitrary instruction
  points; the marker-adoption channel exists precisely for the
  between-phases window (spawn → record), and the phase set includes
  the boundaries around it.
- **Postgres time is initdb time** — on a quieter disk the stackpg
  numbers would compress toward the others.
- **Health checks can be raced** by a squatter binding between the
  pre-flight check and the service's own bind; `verify` detects the
  result after the fact. Closing the race needs socket-ownership
  checks (mapping listen inodes to pids), left as future work.

## 5. Tests

`go test ./...` — 10 tests, all passing: spec parsing/validation/
rendering, registry roundtrip + concurrent-update serialization under
flock, `/proc` stat parsing (including comm-with-spaces), PID-reuse
guard, listen-table parsing, and a full integration test (provision →
verify clean → kill a service → verify flags exactly it → fresh-process
teardown → audit empty → teardown again as a no-op).
