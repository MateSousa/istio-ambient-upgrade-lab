# istio-ambient-upgrade-lab

A self-contained [kind](https://kind.sigs.k8s.io/) lab that stands up an **Istio
ambient** mesh delivered the same way a real GitOps platform delivers it — a
genericized **umbrella chart** wrapping the four upstream Istio subcharts
(`base` + `cni` + `istiod` + `ztunnel`) at one pinned version, published to a
private OCI registry, and reconciled by **ArgoCD** through an **app-of-apps**
that points back at this repo.

This is **slice 1**: the skeleton. It brings the mesh up and proves a test pod
is genuinely enrolled in the ztunnel datapath. Later slices add pgbouncer + DB
clients, a waypoint, a load generator, and the actual upgrade drop-measurement.
Everything here is pure upstream mechanics — `docker.io/istio` images, upstream
Helm charts, zero private identifiers.

## What it builds

```
kind (1 control-plane + 2 workers, k8s 1.33.7 pinned by digest)
  └── ArgoCD (vendored, pinned v3.3.12)
        └── demo-root  (app-of-apps, watches apps/)
              ├── mesh         -> umbrella istio chart 1.0.0 from GHCR OCI (istio-system)
              └── demo-hello   -> ambient-enrolled demo namespace + hello pod (demo-app)
```

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
make up                          # kind + publish chart + ArgoCD + app-of-apps + verify
make verify                      # re-run the convergence + datapath gates
make down                        # delete the kind cluster
make argocd-ui                   # port-forward the ArgoCD UI (pw: make argocd-password)
make scan                        # hygiene: fail on any proprietary identifier
```

`make up` runs, in order: preflight (asserts `GHCR_TOKEN`, docker) → `kind
create` → `publish-chart` → install ArgoCD + register the private GHCR OCI
repository secret → apply the root app-of-apps → wait for the mesh → verify.

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
10. The hygiene scan finds no proprietary identifiers.

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
  mesh/                  mesh.yaml (umbrella) + demo-hello.yaml (later sync-wave)
  data/ apps/ observability/   .gitkeep placeholders for later slices
demo/hello/              ambient-enrolled namespace + hello Deployment + Service
kind/cluster.yaml        1 control-plane + 2 workers, k8s 1.33.7 by digest
scripts/                 up / down / publish-chart / wait-mesh / verify / no-identity-scan
Makefile
```
