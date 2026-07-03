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
              └── app-c        -> ambient Go/pgx client, chain tail (wave 4)
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
12. The hygiene scan finds no proprietary identifiers.

## `targetRevision` is pinned here (on purpose)

The `mesh` Application pins `targetRevision: 1.0.0` exactly. The production model
**floats** a bounded range (`">=1.0.0 <2.0.0"`) so a freshly published chart
auto-syncs — that floating/auto-sync behaviour, and the atomic-sync hazard it
creates during an upgrade, is introduced in the **upgrade-scenario** slice, where
it is the thing under test. Two consequences to know:

- Re-publishing the **same** version under a fixed pin does **not** trigger a
  re-pull. To force ArgoCD to re-pull during iteration, bump to a pre-release
  like `1.0.0-dev1` (chart version + `targetRevision` together).

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
  observability/         .gitkeep placeholder for a later slice
demo/hello/              ambient-enrolled namespace + hello Deployment + Service
demo/data/              demo-data namespace + postgres + pgbouncer writer/reader
demo/gateway-api/        vendored upstream Gateway API v1.2.1 standard CRDs
demo/waypoint/           tenant-waypoint Gateway + HPA + PDB (+ telemetry.yaml.disabled)
demo/app-a/             app-a manifests (top level) + Node build context (app/)
demo/app-b/             app-b manifests + route/DR + Python build context (app/)
demo/app-c/             app-c manifests + route/DR + Go build context (app/)
kind/cluster.yaml        1 control-plane + 3 workers, k8s 1.33.7 by digest
scripts/                 up / down / publish-chart / build-app-images / gen-scram /
                         wait-mesh / ensure-gatewayclass / verify / verify-data /
                         verify-topology / no-identity-scan
Makefile
```
