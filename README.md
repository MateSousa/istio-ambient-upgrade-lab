# istio-ambient-upgrade-lab

A self-contained [kind](https://kind.sigs.k8s.io/) lab that stands up an Istio
ambient mesh the way a GitOps platform would deliver it — a genericized umbrella
chart wrapping the four upstream Istio subcharts (`base` + `cni` + `istiod` +
`ztunnel`) at one pinned version, published to a private OCI registry (GHCR) and
reconciled by ArgoCD through an app-of-apps. On top of that it runs a small
data-path topology and a Go harness that measures whether a ztunnel upgrade drops
connections. Everything uses pure upstream images (`docker.io/istio`,
`docker.io/library/postgres`, `docker.io/edoburu/pgbouncer`,
`docker.io/library/{node,python,golang}`, the upstream Istio Helm charts, and the
upstream Gateway API v1.2.1 CRDs) — no private identifiers.

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

The three clients chain `app-a → app-b → app-c` over HTTP through the mesh: the
load gen drives `app-a`'s `GET /chain`, which does its own SELECT then calls
`app-b`, which does its own SELECT then calls `app-c`. Each east-west hop is L4 +
mTLS via ztunnel and L7 via the tenant waypoint (an Envoy `Gateway` with HPA, PDB,
and per-upstream `DestinationRule`s). Postgres lives in `demo-data` and is
deliberately kept out of the mesh, so the only in-mesh database hop is
`app-a → pgbouncer`. The umbrella chart sets only behavioural knobs (ambient
profiles for cni/istiod, ztunnel `terminationGracePeriodSeconds: 120`); images are
not overridden, so each subchart resolves its own `appVersion` (1.29.2).

## Prerequisites

- **docker** (Docker Desktop or equivalent), running.
- **kind** v0.31.0 (the k8s 1.33.7 node image is pinned by digest for this release).
- **kubectl**, **helm** (v3, OCI support), **curl**, **python3**, **go** (the harness).
- A **GitHub PAT** exported as `GHCR_TOKEN`, with `write:packages` (to publish the
  umbrella chart) and `read:packages` (so ArgoCD can pull the private chart). It is
  read only from the environment, never committed; the ArgoCD OCI repository Secret
  is rendered from `gitops/argocd/install/ghcr-repo-secret.example.yaml` into a
  gitignored file at apply time.

## Quick start

```bash
export GHCR_TOKEN=ghp_...     # PAT with write:packages + read:packages

make up            # kind + publish chart + build images + ArgoCD + app-of-apps + verify
make verify        # re-run the convergence + datapath + data-path + topology + observability gates
make measure       # fire a ztunnel upgrade trigger and measure drops
make down          # delete the kind cluster
make help          # list every target
```

`make up` runs, in order: preflight (asserts `GHCR_TOKEN`, docker) → `kind create`
→ publish chart → install ArgoCD + register the private GHCR OCI repository secret
→ build + `kind load` the app images → apply the root app-of-apps → wait for the
mesh + data path → verify. Each `verify` gate prints `PASS`/`FAIL`; a single
failure fails the run.

### Upgrade scenarios

Five scripted scenarios (`scripts/scenarios/`) exercise the drill end to end
against a running lab. `make scenarios-check` (shellcheck + `bash -n`) is a
hermetic check that needs no cluster; the runs themselves need a cluster and, except
`drain`, `GHCR_TOKEN`.

| Target | Hop | Needs | Expected |
|---|---|---|---|
| `make scenario-drain` | — | cluster | PASS — zero-drop on the drained node |
| `make scenario-patch` | patch `1.29.2→1.29.5` | cluster + GHCR | PASS |
| `make scenario-atomic-sync` | — | cluster + GHCR | PASS — reproduces then mitigates the floating auto-sync hazard |
| `make scenario-minor` | minor `1.29→1.30` | cluster + GHCR | measured (PASS not required); run last |
| `make scenario-reset` | rollback to `1.29.2` | cluster + GHCR | PASS |

`scenario-drain` cordons the target node, suspends `selfHeal` on the `probe`/`load`
Applications, scales that node's echo/probe/load Deployments to 0, then rolls
ztunnel — so the drained node's roll resets zero attributable connections while the
other nodes take their normal bounded resets. The patch/minor/atomic-sync/reset
scenarios commit and push to `main`: `demo-root` tracks `HEAD`, so the mesh only
rolls when the published chart and the `targetRevision` bump both land on the branch
ArgoCD watches. Each run mints one more immutable GHCR chart version (prune later via
the GHCR UI or `gh api`). The minor hop touches CRDs and can wedge the cluster
(Gateway API v1.2.1 vs istio 1.30 CRD-compat plus the ArgoCD prune/selfHeal cascade);
its guaranteed recovery is `make down && make up`.

### Observability (optional)

The `observability` layer (sync-wave 5) stands up Prometheus, Loki, Grafana, and
Alloy as public Helm-repo ArgoCD Applications, kept out of the mesh so their
scrape/query traffic never routes through ztunnel and muddies the L4 signal. The
harness (`results.json`) is the source of truth for the drill's verdict; Grafana is
corroboration. Note that `istio_tcp_sockets_open` is not a native ztunnel 1.29.2
series, so a sockets-open view uses an `increase()`-based recording rule
(`lab:tcp_sockets_open`) that survives a ztunnel pod's counters expiring across a
roll — see `apps/observability/prometheus.yaml`.

```bash
make grafana-ui            # Grafana    -> http://localhost:3001 (admin/admin)
make prometheus-ui         # Prometheus -> http://localhost:9090
make loki-ui               # Loki HTTP API -> http://localhost:3100
make verify-observability  # run only the observability gates
```

## Hygiene scanner

This is a genericized, public reproduction of an internal upgrade drill, so a
scanner guards the tree against leaking any private identifier. It is a tested Go
module (`harness/internal/scan`) run via `harness scan`; the shell entry points are
thin wrappers over it. `make scan` (and the gate inside `make verify`) exits 0 on a
clean tree and non-zero — printing every `file:line` — on any hit or if it cannot
run (fail-closed). `make install-hooks` points `core.hooksPath` at `.githooks`,
whose opt-in `pre-push` hook runs the scan.

## Layout

- `charts/istio/` — umbrella chart (base/cni/istiod/ztunnel @ 1.29.2).
- `gitops/argocd/` — vendored ArgoCD install (v3.3.12), OCI repo-secret template, and the `demo-root` root app-of-apps.
- `apps/` — the ArgoCD child Applications (mesh, data, waypoint, app-a/b/c, probe, load, observability), grouped by sync wave.
- `demo/` — the manifests and build contexts the Applications point at: hello, data (postgres + pgbouncer), gateway-api CRDs, waypoint, observability dashboard, and the app-a/b/c sources.
- `harness/` — the Go drop-measurement harness; `harness next-version` is the single fresh-chart-version authority the scenarios call, and `harness scan` is the hygiene scanner.
- `scripts/` — `up`/`down`, `publish-chart`, `build-app-images`, `gen-scram`, `wait-mesh`, `ensure-gatewayclass`, the `verify-*` gates, `no-identity-scan`, and `scenarios/` (the shared lib + drain/patch/atomic-sync/minor/reset).
- `kind/cluster.yaml` — 1 control-plane + 3 workers, k8s 1.33.7 by digest.

## Notes / caveats

- **`targetRevision` is pinned here.** The `mesh` Application pins
  `targetRevision: 1.0.0` exactly. Re-publishing the same version under a fixed pin
  does not trigger a re-pull — bump to a pre-release like `1.0.0-dev1` (chart version
  + `targetRevision` together) to force one. Production floats a bounded range
  (`">=1.0.0 <2.0.0"`) so a fresh chart auto-syncs; that floating behaviour is
  exercised only in the atomic-sync scenario, where it is the thing under test.
- **Docker Desktop inotify limits.** `fs.inotify.max_user_instances` and
  `fs.inotify.max_user_watches` live in the Docker Desktop LinuxKit VM, not on the
  macOS host — `sysctl` on the Mac does not change them. Many pods, collectors, and
  the load gen can exhaust them and stall pod/informer startup; raise them inside the
  VM if pods hang in `ContainerCreating`.
- **kubectl / server version skew.** A very new client (e.g. kubectl 1.36) against
  the 1.33 API server is one minor outside the supported ±1 window. It works for
  everything used here, but expect occasional discovery warnings.
- **ztunnel `grace=120` == kubelet critical-pod grace.** ztunnel's 120s
  `terminationGracePeriodSeconds` equals the kubelet `shutdownGracePeriodCriticalPods`
  default — zero margin. A node consolidation/shutdown mid-drain can truncate
  ztunnel's drain and cut connections early.
- **Private GHCR package.** The chart package is private; ArgoCD needs the
  `read:packages` half of the PAT to pull it. A `read:packages`-less token publishes
  fine but the `mesh` app then fails to pull.
- **Observability egress.** ArgoCD's repo-server must reach
  `prometheus-community.github.io` and `grafana.github.io` (not mirrored to the
  private registry) to pull the observability charts; `scripts/up.sh` warns rather
  than hard-fails if they are unreachable, since the mesh and app layers come up
  without them.
```
