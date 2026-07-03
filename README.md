# istio-ambient-upgrade-lab

A self-contained [kind](https://kind.sigs.k8s.io/) lab that stands up an **Istio
ambient** mesh delivered the same way a real GitOps platform delivers it — a
genericized **umbrella chart** wrapping the four upstream Istio subcharts
(`base` + `cni` + `istiod` + `ztunnel`) at one pinned version, published to a
private OCI registry, and reconciled by **ArgoCD** through an **app-of-apps**
that points back at this repo.

**Slice 1** brought the mesh up and proved a test pod is genuinely enrolled in
the ztunnel datapath. **Slice 2** added the data path (out-of-mesh Postgres +
in-mesh pgbouncer + **app-a** Node/TypeORM). Slices 3–4 added the
drop-measurement harness and the concurrent load generator. **Slice 5 (this)
thickens the workload to the full topology**: **app-b** (Python/SQLAlchemy +
psycopg3) and **app-c** (Go/pgx) join app-a — three deliberately-distinct pool
implementations, each ambient-enrolled and each holding its own pgbouncer-fronted
pool — chained `app-a → app-b → app-c` over HTTP through the mesh, plus a
**tenant waypoint** (L7 Envoy) that east-west traffic to app-b/app-c is routed
through. This exercises both the L4 (ztunnel) and L7 (waypoint) paths so a later
upgrade run can measure per-client RST/recovery differences. Everything here is
pure upstream mechanics — `docker.io/istio`, `docker.io/library/postgres`,
`docker.io/edoburu/pgbouncer`, `docker.io/library/{node,python,golang}`, the
upstream Istio Helm charts, and the upstream **Gateway API v1.2.1** standard CRDs
— zero private identifiers.

## What it builds

```
kind (1 control-plane + 3 workers, k8s 1.33.7 pinned by digest)
  └── ArgoCD (vendored, pinned v3.3.12)
        └── demo-root  (app-of-apps, watches apps/)
              ├── gateway-api-crds -> upstream Gateway API v1.2.1 standard CRDs (wave -1)
              ├── mesh         -> umbrella istio chart 1.0.0 from GHCR OCI (wave 0)
              ├── demo-hello   -> ambient-enrolled demo namespace + hello pod (wave 1)
              ├── data         -> postgres (demo-data, OUT of mesh) + pgbouncer
              │                   writer/reader pools (demo-app, in mesh) (wave 2)
              ├── waypoint     -> tenant-waypoint L7 Envoy: Gateway + HPA + PDB (wave 3)
              ├── probe / load -> per-node drop-measurement probes + load gen (wave 3/4)
              ├── app-a        -> ambient Node/TypeORM client, chain head (wave 4)
              ├── app-b        -> ambient Python/SQLAlchemy client, chain middle (wave 4)
              ├── app-c        -> ambient Go/pgx client, chain tail (wave 4)
              └── observability (wave 5, OUT of mesh, public Helm repos)
                    ├── prometheus  -> ztunnel/istiod/waypoint metrics (server-only)
                    ├── loki        -> log store (SingleBinary, filesystem)
                    ├── alloy       -> DaemonSet: tails node logs -> Loki
                    ├── grafana     -> Prometheus + Loki datasources + starter dashboard
                    └── observability-config -> the dashboard ConfigMap (demo/observability)
```

The three services chain `app-a → app-b → app-c` over HTTP through the mesh: the
load gen drives app-a's `GET /chain`, which does its own SELECT then calls
app-b's `/chain`, which does its own SELECT then calls app-c's `/query`. Every
east-west hop is L4 + mTLS via ztunnel and L7 via the **tenant waypoint** (HBONE
listener `15008`, `for: service`, HPA min 2 / max 4 / 70% CPU, PDB
`minAvailable: 1`, and per-upstream DestinationRules: `idleTimeout 55s`,
`maxRequestsPerConnection 1000`, LEAST_REQUEST, outlier detection). app-b/app-c
Services carry `istio.io/use-waypoint: tenant-waypoint` (value == the Gateway
name) and each has a GAMMA HTTPRoute attaching it to the waypoint. app-b/app-c
use **soft** (preferred) anti-affinity only — app-a keeps its required rule; the
verdict signal is the per-node probe/echo pairs, not the app pools.

The drop-relevant path is `app-a -> pgbouncer` (in mesh, carried over HBONE by
ztunnel) and `pgbouncer -> postgres` (out of mesh, plaintext) — matching
production, where only the app -> pooler hop is in the mesh. app-a is pinned by
REQUIRED anti-affinity to a different node than every pgbouncer-writer, so that
in-mesh hop is a deterministic cross-node connection. pgbouncer replicates the
nonprod pooler verbatim: `pool_mode=transaction`, the documented pool
sizes/timeouts, `replicas: 2`, RollingUpdate `maxUnavailable:0/maxSurge:1`,
`terminationGracePeriodSeconds: 150`, and a PDB `minAvailable: 1` per pool.
app-a's local image is built and `kind load`ed before the app-of-apps syncs
(`scripts/build-app-images.sh`, `imagePullPolicy: IfNotPresent`, no registry
prefix).

The umbrella chart (`charts/istio`) sets only behavioural knobs — ambient
profiles for cni/istiod and ztunnel `terminationGracePeriodSeconds: 120`. Images
are **not** overridden, so each subchart resolves its own `appVersion` (1.29.2)
against docker.io/istio.

## Observability (Prometheus + Loki + Grafana)

The `observability` layer (sync-wave **5**, after the apps so its scrape targets
exist) stands up the Grafana OSS stack the same way the real drill is validated
in SigNoz — metrics graphs plus queryable error logs. It is delivered as four
**public Helm-repo** ArgoCD Applications (exact pins, **not** republished to the
private GHCR registry) plus one directory app for the dashboard:

| App | Chart (pin) | Shape |
|---|---|---|
| `prometheus` | `prometheus-community/prometheus` `27.52.0` | server **only** (no alertmanager/node-exporter/kube-state-metrics/pushgateway), 1 replica, emptyDir, retention **2h**, scrape **30s** |
| `loki` | `grafana/loki` `6.55.0` | SingleBinary, filesystem/emptyDir, tsdb schema v13, caches/minio/gateway off, retention 24h |
| `alloy` | `grafana/alloy` `1.10.0` | DaemonSet, tails `/var/log/pods` → Loki, labels `namespace/pod/container/app/node` |
| `grafana` | `grafana/grafana` `8.15.0` | 1 replica, no persistence, `admin/admin` (trivial lab cred), Prometheus+Loki datasources auto-provisioned, dashboard sidecar (searchNamespace `observability`) |

The `observability` namespace is created with `CreateNamespace=true` and is
**deliberately NOT ambient-enrolled** (no `istio.io/dataplane-mode` label): the
stack observes the mesh from *outside* it, so its own scrape/query traffic never
routes through ztunnel/HBONE and never muddies the L4 signal under measurement.

**Scrape config is static** (`serverFiles` chart values, `kubernetes_sd` role
`pod`) — there is no prometheus-operator / ServiceMonitor in this lab. Three jobs:
`ztunnel` (`app=ztunnel` in `istio-system`, `:15020 /stats/prometheus`), `istiod`
(`:15014 /metrics`), `waypoint` (`gateway.networking.k8s.io/gateway-name=tenant-waypoint`
in `demo-app`, `:15020 /stats/prometheus`).

### Honest metric names (what is real vs lab-synthesized)

This is the SigNoz-parity **view**, so the metric names matter. Only **real
upstream series** are emitted; anything synthesized is named distinctly so nobody
mistakes it for a native Istio metric.

| Signal (dashboard) | PromQL / source | Real or lab? |
|---|---|---|
| TCP opened/sec, closed/sec | `istio_tcp_connections_opened_total`, `istio_tcp_connections_closed_total` | **Real** (native ztunnel 1.29.2) |
| TCP bytes sent/received | `istio_tcp_sent_bytes_total`, `istio_tcp_received_bytes_total` | **Real** (native ztunnel) |
| istiod XDS clients | `pilot_xds` | **Real** (istiod) |
| istiod push latency p95 | `pilot_xds_push_time_bucket` (suffix pinned at live bring-up) | **Real** (istiod) |
| istiod XDS rejects | `pilot_total_xds_rejects` | **Real** (istiod) |
| waypoint requests by code | `istio_requests_total`, `istio_request_duration_milliseconds` | **Real** (waypoint Envoy) |
| **sockets open** | `lab:tcp_sockets_open` **recording rule** | **LAB-synthesized** |

`istio_tcp_sockets_open` and `istio_tcp_connections_failed_total` are **not**
native ztunnel 1.29.2 series, so they are **not** emitted under those names. For a
sockets-open view, a Prometheus **recording rule** `lab:tcp_sockets_open` is built
from `increase()`-based expressions instead of a bare
`sum(opened) - sum(closed)`. Why: a ztunnel roll **deletes** the old ztunnel pod,
so its per-pod counters **expire** from Prometheus — a bare cumulative subtraction
then shows a **false cliff exactly at the measured event** (an artifact of the
vanished series, not a real drop). `increase()` over a trailing window is reset-
and disappearance-tolerant, so the rule degrades gracefully across the roll. See
`apps/observability/prometheus.yaml` for the full comment.

### The harness is the source of truth — Grafana is corroboration

**The drill's verdict metrics (new-connection failures, existing-connection RSTs,
recovery) come from the slice-3 HARNESS (`results.json`), NOT from Prometheus.**
Prometheus/Grafana is the operator-facing SigNoz-parity **view** — corroboration
and situational awareness — not the authority for PASS/FAIL. The harness measures
at the socket (ECONNRESET vs FIN) and decides; the dashboard shows the shape.

### Access logs require a chart republish

The "app AND Istio access logs" requirement has two halves with different timing:

- **App logs** flow **now**: the apps already log to stdout, Alloy tails them into
  Loki, and `{namespace="demo-app"} |~ "ECONNRESET|Connection terminated"` is
  queryable as soon as a drop occurs.
- **Istio access logs** need a **chart republish**: `meshConfig.accessLogFile:
  /dev/stdout` (JSON) is set on the umbrella chart (`charts/istio/values.yaml`)
  and the waypoint `Telemetry` (`demo/waypoint/telemetry.yaml`, re-pointed from the
  removed tracing placeholder to **access logging**) is enabled — but a meshConfig
  change only takes effect when the chart is **republished** (bump to `1.0.0-dev2`
  + move the `mesh` Application's `targetRevision` pin), which happens on `make up`
  with `GHCR_TOKEN`. So the **collection path (Alloy → Loki) is ready now**; Istio
  access-log **generation activates on republish**. This is not claimed as live
  without that republish.

### Network dependency

ArgoCD's repo-server must reach the **public Helm repos**
`prometheus-community.github.io` and `grafana.github.io` to pull these charts (they
are *not* mirrored to the private registry). `scripts/up.sh` runs a `curl -sfI`
reachability check and warns (does not hard-fail — the mesh + app slices come up
without observability) if they are unreachable.

```bash
make grafana-ui       # port-forward Grafana  -> http://localhost:3001 (admin/admin)
make prometheus-ui    # port-forward Prometheus -> http://localhost:9090
make loki-ui          # port-forward Loki HTTP API -> http://localhost:3100
make verify-observability   # run only the observability gates
```

## Prerequisites

- **docker** (Docker Desktop or equivalent) — running.
- **kind** v0.31.0 (the k8s 1.33.7 node image is pinned by digest for this kind release).
- **kubectl**, **helm** (v3, OCI support), **curl**, **python3** (used by the verify script).
- A **GitHub PAT** exported as `GHCR_TOKEN`, with:
  - `write:packages` — to publish the umbrella chart to `ghcr.io/matesousa/charts`.
  - `read:packages` — so ArgoCD can pull the **private** chart.

  The token is only ever read from the environment. It is never committed; the
  ArgoCD OCI repository Secret is rendered from a template
  (`gitops/argocd/install/ghcr-repo-secret.example.yaml`) into a gitignored file
  at apply time.

## Usage

```bash
export GHCR_TOKEN=ghp_...        # PAT with write:packages + read:packages
make up                          # kind + publish chart + build images + ArgoCD + app-of-apps + verify
make build-images                # build + kind-load the demo app images (app-a)
make verify                      # re-run the convergence + datapath + data-path gates
make down                        # delete the kind cluster
make argocd-ui                   # port-forward the ArgoCD UI (pw: make argocd-password)
make scan                        # hygiene: fail on any proprietary identifier
make install-hooks               # opt-in: run the hygiene scan on every git push
```

`make up` runs, in order: preflight (asserts `GHCR_TOKEN`, docker) → `kind
create` → `publish-chart` → install ArgoCD + register the private GHCR OCI
repository secret → build + `kind load` the app images → apply the root
app-of-apps → wait for the mesh + data path → verify.

The SCRAM-SHA-256 verifier literals shared by Postgres and pgbouncer are
regenerated with `python3 scripts/gen-scram.py` (fixed salts, lab-only).

## What gets proven (`make verify`)

Each gate prints `PASS`/`FAIL`; a single failure fails the run.

1. All nodes `Ready`.
2. ArgoCD `argocd-server` + `argocd-repo-server` rolled out.
3. `demo-root` and `mesh` Applications are `Synced`/`Healthy`.
4. The `mesh` Application source is a **GHCR** repoURL and its resolved revision
   is exactly **1.0.0**.
5. `istiod` has an available replica.
6. `ztunnel` **and** `istio-cni-node` DaemonSets are fully ready — asserted from
   status (`desiredNumberScheduled == numberReady`, `numberUnavailable == 0`),
   **not** a hardcoded node count, so it adapts to any worker count.
7. `ztunnel` `terminationGracePeriodSeconds` is `120` on the live DaemonSet.
8. `reconcileIptablesOnStartup` is not disabled (1.29 default `true` — the CNI
   roll's no-drop precondition).
9. **Datapath enrollment (the load-bearing gate):** the `hello` pod's IP appears
   in the `config_dump` of the **ztunnel pod on the same node** (read via a
   port-forward to that ztunnel's admin port `15000`). This proves the pod is in
   the mesh datapath — a labelled, `Running` pod alone is *not* accepted as proof.
10. **Data-path gates** (`scripts/verify-data.sh`):
    - `postgres` Running in `demo-data`, which carries **no** `dataplane-mode`
      label.
    - **Out-of-mesh proof:** in every ztunnel `config_dump` — the *same* dump
      that *does* contain the in-mesh pgbouncer + app-a IPs (a positive control
      that the dump is populated) — the Postgres pod IP is **absent**.
    - `pgbouncer-writer` + `app-a` IPs present in their nodes' ztunnel datapath.
    - `app-a` `/readyz` is 200 and `/query` returns a `widgets` row (app →
      pgbouncer → Postgres, end to end).
    - app-a holds a long-lived pooled client, visible in the pgbouncer admin
      console (`SHOW CLIENTS` / `SHOW POOLS`) — not just a `pg_stat_activity` row.
    - `pool_mode=transaction` (live `SHOW CONFIG`) and
      `terminationGracePeriodSeconds=150` on both pools.
    - `app-a` and `pgbouncer-writer` are on **different** nodes (anti-affinity).
11. **Topology gates** (`scripts/verify-topology.sh`):
    - `GatewayClass/istio-waypoint` is **Accepted** (the CRD-registration gate;
      `scripts/ensure-gatewayclass.sh` rolls istiod if an incremental apply left
      it unregistered).
    - `app-b` + `app-c` are enrolled in their nodes' ztunnel datapath, and each
      serves `/readyz` + `/query` with a `widgets` row.
    - both are persistent `demo_app` clients in pgbouncer (`SHOW CLIENTS` by
      `application_name`) — each holds its own pool.
    - `app-a` `GET /chain` returns rows stitched from a → b → c.
    - the Gateway `tenant-waypoint` is **Programmed=True** (what ArgoCD's Gateway
      health keys on) and its auto-managed Envoy Deployment has ≥ 2 replicas.
    - `app-b` + `app-c` Services carry `istio.io/use-waypoint` == the Gateway name.
    - the per-upstream DestinationRules exist with the `55s`/`1000`/LEAST_REQUEST
      timeouts.
    - **L7 traversal proof:** the waypoint's `istio_requests_total` increments
      when `/chain` is driven (port-forward to the waypoint's `15020` metrics).
    - Postgres is **still** out of mesh (its IP absent from every ztunnel dump).
12. **Observability gates** (`scripts/verify-observability.sh`):
    - the `prometheus`/`loki`/`alloy`/`grafana`/`observability-config` Applications
      are `Synced`/`Healthy` (retried — they sync on wave 5).
    - Prometheus `/api/v1/targets` has an **UP** target for the `ztunnel` + `istiod`
      jobs (waypoint is **best-effort/retried**, not hard-failed before wave 3/4
      converge).
    - Prometheus `/api/v1/query` returns data for the **real** names
      (`istio_tcp_connections_opened/closed_total`, `pilot_xds`,
      `pilot_total_xds_rejects`) **and** the `lab:tcp_sockets_open` recording rule.
    - Loki is **ingesting** (namespace label values include `demo-app` +
      `istio-system`; a broad query returns > 0 streams) and the
      `ECONNRESET|Connection terminated` LogQL **executes** (200 — may be empty
      until a drop occurs; gated on ingestion + query-executes, not non-empty).
    - Grafana `/api/health` is ok, both datasources are provisioned, and the
      starter dashboard is present.
13. The hygiene scan finds no proprietary identifiers.

## Hygiene scanner (no proprietary identifiers)

This is a genericised, public reproduction of an internal upgrade drill, so a
scanner guards the tree against leaking any private fingerprint — an internal
org name, the internal AWS account IDs, an internal container-registry host, an
internal collector FQDN/namespace, or an IRSA ARN shape. It is a tested Go deep
module (`harness/internal/scan`) run via `harness scan`; the shell entry points
are thin wrappers so there is a single implementation that cannot drift.

- `make scan` (and the hygiene gate inside `make verify`) runs it. It exits
  **0** on a clean tree and **non-zero**, printing every `file:line`, on any hit
  or if it cannot run (fail-closed: it never passes an unscanned tree).
- By default it scans the **git-tracked** file set — exactly what a push would
  publish — and automatically falls back to walking the working tree when git is
  absent (tarball/CI). `harness scan --worktree` forces the walk and includes
  untracked files.
- The patterns live in one fragment-assembled rule file that the scan excludes
  from itself (a deliberately narrow exclusion — a real secret in any other path
  is still caught). Every other file, including the tests, is fingerprint-free:
  the positive fixture corpus is assembled from fragments at test time into a
  temp dir, so no identifier is ever written to a committed file on disk.
- `make install-hooks` points `core.hooksPath` at `.githooks`, whose `pre-push`
  hook runs the scan and blocks a push on any finding. It is **opt-in per clone**
  and bypassable with `git push --no-verify`; `make scan` and CI are the real
  guarantee.

## `targetRevision` is pinned here (on purpose)

The `mesh` Application pins `targetRevision: 1.0.0` exactly. The production model
**floats** a bounded range (`">=1.0.0 <2.0.0"`) so a freshly published chart
auto-syncs — that floating/auto-sync behaviour, and the atomic-sync hazard it
creates during an upgrade, is introduced in the **upgrade-scenario** slice, where
it is the thing under test. Two consequences to know:

- Re-publishing the **same** version under a fixed pin does **not** trigger a
  re-pull. To force ArgoCD to re-pull during iteration, bump to a pre-release
  like `1.0.0-dev1` (chart version + `targetRevision` together).

## Upgrade scenarios (slice 8)

Five scripted scenarios (`scripts/scenarios/`) exercise the full drill end to end
against the running lab. Each sets `set -euo pipefail`, installs a trap-guarded
restore, and prints `PASS`/`FAIL` gates. `make scenarios-check` (shellcheck +
`bash -n`) is the **hermetic** CI entry — it needs no cluster; the scenario
**runs** need a cluster (and, except drain, `GHCR_TOKEN`).

| `make` target | Hop | Needs | Expected |
|---|---|---|---|
| `scenario-drain` | — | cluster | **PASS** — zero-drop on the drained node |
| `scenario-patch` | patch `1.29.2→1.29.5` | cluster + GHCR | **PASS** |
| `scenario-atomic-sync` | — | cluster + GHCR | **PASS** — reproduces then mitigates the hazard |
| `scenario-minor` | minor `1.29→1.30` | cluster + GHCR | **measured** (PASS *not* required) — run last |
| `scenario-reset` | rollback to `1.29.2` | cluster + GHCR | **PASS** |

### Git writes to `main` (by design)

A throwaway branch is **off the table**: `demo-root` tracks `HEAD` and the `mesh`
Application only rolls when the published chart **and** the `targetRevision` bump
land on the branch ArgoCD watches (`main`). A bump stranded on a side branch
produces no roll → the analyzer reports `ERROR no-rollout-observed`. So `patch`,
`minor`, `atomic-sync`, and `reset` **commit + push to `main`** and each
accumulates one more **immutable** GHCR chart version (prune later via the GHCR
UI or `gh api` if the registry gets noisy). The **fresh version is minted by one
authority** — `harness next-version` — so a run never invents a version string in
shell (it always carries a mandatory `-dev<timestamp>` prerelease and stays
`<2.0.0`).

### Drain = zero drops (honest framing + scoped claim)

`scenario-drain` **drains the connections off a node before rolling its ztunnel**,
so **zero RSTs are attributable to that node**. It is *deterministic*, not a
`kubectl drain`: the probe/echo/load pods are pinned by `nodeName` (which bypasses
the scheduler **by design**), so a plain drain would refuse to evict them or leave
them `Pending`. Instead it **cordons**, **suspends `selfHeal` on the `probe` *and*
`load` Applications** (required — else ArgoCD reverts the scale-to-0 back to
`replicas:1` from `HEAD`), **scales the target node's echo/probe/load Deployments
to 0** and `wait --for=delete`s them, then fires the roll. The proof is a hard
assertion on `results.json`: the drained node's `perNodeAttribution` entry **must
exist**, be **closed** (`window.readyAt != null`, not half-open), and have
**`distinctConnsReset == 0`**; the script prints that `0` against the other nodes'
normal bounded resets as the contrast. **No pods are left `Pending`** — `nodeName`
bypasses the scheduler. **Scoped claim:** zero-drop covers the mesh workloads we
control (`probe`/`load`/`echo`). `app-a`/`app-b`/`app-c` are ordinary schedulable
clients, not harness probes; one may sit on the target node and reset elsewhere —
that is invisible to the verdict and **not** part of the claim.

### Atomic sync (the contrast)

`scenario-atomic-sync` first **reproduces** the hazard — float `targetRevision` to
`">=1.0.0 <2.0.0"` with `selfHeal`, publish a fresh chart, and watch ArgoCD
auto-deploy it **unbidden** (counted via the `mesh` Application's sync history) —
then **mitigates** it: re-pin to an exact version and bump the chart version **and**
`targetRevision` **together in one commit**, yielding a single operator-gated roll.

### Minor is terminal

`scenario-minor` is **run last**. A minor hop crosses a minor, is governed by the
skew rule, and **touches CRDs**: the vendored **Gateway API v1.2.1** CRDs and
istio 1.30's expected CRD set can end up mutually incompatible, and ArgoCD
`prune`/`selfHeal` make a CRD change a **cascade** (a renamed/removed CRD can prune
every CR of that kind). Because of that cascade, `scenario-reset` may **not**
un-wedge a minor-hopped cluster. The **guaranteed** recovery — and what `reset`'s
header points you to — is a full rebuild:

```bash
make down && make up
```

## Known issues / gotchas

- **Docker Desktop inotify limits.** `fs.inotify.max_user_instances` and
  `fs.inotify.max_user_watches` live in the Docker Desktop **LinuxKit VM**, not
  on the macOS host — `sysctl` on the Mac does not change them. This slice is
  light enough to be fine, but the fuller lab (many pods, collectors, load gen)
  can exhaust them and stall pod/informer startup. Raise them inside the VM if
  pods hang in `ContainerCreating`.
- **kubectl / server version skew.** A very new client (e.g. kubectl 1.36)
  against the 1.33 API server is one minor outside the officially supported
  ±1-minor skew window. It works for everything used here, but expect occasional
  discovery warnings.
- **`grace=120` == kubelet critical-pod grace.** ztunnel's 120s
  `terminationGracePeriodSeconds` equals the kubelet
  `shutdownGracePeriodCriticalPods` default — zero margin. It doesn't bite this
  skeleton, but it matters in the upgrade slices: a node consolidation/shutdown
  mid-drain truncates ztunnel's drain and can cut connections early.
- **Private GHCR package.** The chart package is private; ArgoCD needs the
  `read:packages` half of the PAT to pull it. A `read:packages`-less token
  publishes fine but the `mesh` app then fails to pull.

## Layout

```
charts/istio/            umbrella chart (base/cni/istiod/ztunnel @ 1.29.2)
gitops/argocd/
  install/               vendored ArgoCD install.yaml (v3.3.12) + OCI repo-secret template
  root-app-of-apps.yaml  demo-root Application (watches apps/)
apps/
  mesh/                  gateway-api-crds.yaml (wave -1) + mesh.yaml (0) + demo-hello.yaml (1)
  data/data.yaml         postgres + pgbouncer child app (wave 2)
  apps/waypoint.yaml     tenant-waypoint child app (wave 3)
  apps/{app-a,app-b,app-c}.yaml  the three chained clients (wave 4)
  apps/{probe,load}.yaml drop-measurement probe (wave 3) + load gen (wave 4)
  observability/         prometheus/loki/alloy/grafana Helm-repo apps +
                         observability-config directory app (all wave 5)
demo/hello/              ambient-enrolled namespace + hello Deployment + Service
demo/data/              demo-data namespace + postgres + pgbouncer writer/reader
demo/gateway-api/        vendored upstream Gateway API v1.2.1 standard CRDs
demo/waypoint/           tenant-waypoint Gateway + HPA + PDB + telemetry.yaml (access logging)
demo/observability/      Grafana starter dashboard ConfigMap (grafana_dashboard=1)
demo/app-a/             app-a manifests (top level) + Node build context (app/)
demo/app-b/             app-b manifests + route/DR + Python build context (app/)
demo/app-c/             app-c manifests + route/DR + Go build context (app/)
kind/cluster.yaml        1 control-plane + 3 workers, k8s 1.33.7 by digest
scripts/                 up / down / publish-chart / build-app-images / gen-scram /
                         wait-mesh / ensure-gatewayclass / verify / verify-data /
                         verify-topology / verify-observability / no-identity-scan
  scenarios/             slice-8 upgrade scenarios: scenario-lib (shared helpers) +
                         drain / patch / atomic-sync / minor / reset
Makefile
harness/                 drop-measurement harness (Go); `harness next-version` is
                         the single fresh-umbrella-version authority the scenarios call;
                         internal/scan is the hygiene scanner (`harness scan`)
.githooks/pre-push       opt-in pre-push hygiene gate (make install-hooks)

```
