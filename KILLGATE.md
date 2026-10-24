# envforge — Kill-Gate Review

**Question:** does an available tool already do what envforge claims — a
*local-first* ephemeral per-PR environment provisioner whose guarantees are
*measured*: provision-time distribution, cross-env isolation proof, drift
detection against the spec, and a teardown-completeness proof (provisioner
killed at every phase → independent leak audit finds zero orphans)?

## Prior art

| Tool | What it is | Why it is not this |
|---|---|---|
| **Qovery, Uffizzi, Bunnyshell, Northflank, Atmosly** | Managed ephemeral-env platforms | Cloud/Kubernetes only. Teardown is an event (PR-closed webhook) plus TTL/reaper sweeps. Qovery markets "guaranteed zero orphaned resources" for its hosted control plane, but publishes no leak-audit method and has no local mode. |
| **Heroku Review Apps** | Per-PR app + add-ons on Heroku | App-level only, cloud only, teardown tied to PR close; no drift check, no crash-survival story. |
| **GitHub Actions `services:` / GKE preview tutorials** | CI service containers / DIY namespace-per-PR | CI-scoped or cloud-scoped. A widely read GKE tutorial states the quiet part: even ~95%-effective cleanup *still leaks*, so a scheduled reaper deletes leftovers later. Leaks are accepted and mopped up asynchronously — never proven zero at teardown time. |
| **Terraform / Pulumi** | Declarative IaC, `destroy` from state | Destroy deletes *what the state file says*. If the provisioner dies mid-apply, state is stale or corrupt and teardown no longer knows what exists (well-documented incidents of corrupted state freezing all operations, and destroys acting on the wrong resource set). No local process/port/dir model, no independent audit. |
| **Docker Compose (profiles)** | Local multi-service runner | No per-PR registry, no port-block allocation across envs, no drift verification, `down` is happy-path only: kill Compose mid-`up` and containers/networks/volumes leak until manual cleanup. Requires Docker (absent here). |
| **Testcontainers** | Test-scoped throwaway dependencies (library) | Library, not a per-PR environment manager; requires a Docker daemon; cleanup is a best-effort reaper container (Ryuk). No spec/drift/teardown-proof model. |
| **Tilt / Skaffold / Garden** | Local dev-loop / CI pipeline tools | Optimised for the edit-rebuild loop on one developer's stack; no multi-env registry, no teardown guarantee, no leak audit. |

## The gap

Every incumbent treats teardown as a **happy-path step in the same process
that provisioned**. Correctness therefore depends on the provisioner
surviving, its state file being intact, and a webhook arriving. None of
them — cloud or local — publishes a measured teardown guarantee of the
form: *kill the provisioner at every phase boundary, including mid-provision
and mid-teardown, then run an audit that does not trust the provisioner's
own bookkeeping, and count the orphans.*

## envforge's differentiation

1. **Local-first, $0, no cloud, no Docker** — plain processes on one host:
   per-env Redis and PostgreSQL processes plus app services from a JSON spec.
2. **Registry-first design** — the (locked, atomically-written) registry is
   updated *before/after every step*, and teardown runs entirely from the
   registry from a *fresh* process. A second, independent discovery channel
   (scanning `/proc` for the `ENVFORGE_ENV` marker stamped into every child
   process) lets teardown adopt processes the registry never recorded.
3. **Teardown is proven, not asserted** — a chaos harness `kill -9`s the
   provisioner at every phase and audits for orphaned processes, listening
   ports, data dirs, and database state. Numbers are published in PAPER.md.
4. **Drift is a first-class command** — `verify` compares live state
   (process identity incl. PID-reuse guard, listening ports, config read
   back over HTTP, rendered files, spec hash) against the spec.
5. **Isolation is tested, not assumed** — two envs from the *same* spec run
   concurrently; cross-reads are attempted and interference is measured and
   reported honestly (shared host CPU is a real, named limit).

## Verdict

**PROCEED.** No available tool is a local provisioner with a leak-audited
teardown proof. The incumbents are cloud platforms with best-effort,
reaper-backed cleanup, or local dev tools with no teardown model at all.
