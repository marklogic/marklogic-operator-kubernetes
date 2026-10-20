# Test MarkLogic Kubernetes Operator with e2e-framework

The operator has two e2e test environments: **Minikube** (local, for development)
and **EKS** (the Jenkins CI target).

Start with the [VAC EKS quick test](../docs/volume-attributes-class-eks-quick-test.md)
for main functionality first, then additional checks. For full qualification,
evidence collection, PR gates, and the demo checklist, see the
[VAC EKS runbook](../docs/volume-attributes-class-eks-testing.md). These isolated
workflows avoid the existing e2e harness's CRD/namespace cleanup on shared or
demo clusters.

## Test suites

| Package | Description |
|---|---|
| `test/e2e-helm` | **Namespace-scoped tests (default).** Operator installed via `helm install` with `scope.type=namespace`. Validates that Role/RoleBinding per watched namespace is sufficient and no ClusterRole is present. |
| `test/e2e` | Cluster-scoped tests. Operator deployed via `make deploy` (kustomize, ClusterRole/ClusterRoleBinding). |

---

## test/e2e — Cluster-scoped

### Setup and run

---

## Minikube (local development)

```bash
make e2e-setup-minikube
make e2e-test
make e2e-cleanup-minikube
```

`make e2e-test` runs the **namespace-scoped** suite (`test/e2e-helm`) by default. Set
`E2E_SCOPE=cluster` to run the cluster-scoped suite (`test/e2e`) instead:

```bash
make e2e-test E2E_SCOPE=cluster
```

`e2e-setup-minikube` can also install Istio in ambient mode (`E2E_SETUP_ISTIO=true`,
default), so Istio tests (`make e2e-test-istio`) run against the same cluster —
no separate cluster/shard is required. Tests opt in to Istio per-namespace via
the `istio.io/dataplane-mode: ambient` label rather than by recreating the cluster.
For faster namespace-only or focused non-Istio runs, pass `E2E_SETUP_ISTIO=false`.

### Parallelizing e2e runs with minikube profiles

Every `e2e-setup-minikube*`/`e2e-test-*`/`e2e-cleanup-minikube` target accepts a
`MINIKUBE_PROFILE` variable (default: `minikube`). Use distinct profiles to run
independent suites concurrently against separate minikube clusters:

```bash
make e2e-setup-minikube MINIKUBE_PROFILE=e2e-cluster &
make e2e-setup-minikube MINIKUBE_PROFILE=e2e-namespace &
wait

make e2e-test-cluster MINIKUBE_PROFILE=e2e-cluster &
make e2e-test-helm-namespace MINIKUBE_PROFILE=e2e-namespace &
wait

make e2e-cleanup-minikube MINIKUBE_PROFILE=e2e-cluster
make e2e-cleanup-minikube MINIKUBE_PROFILE=e2e-namespace
```

Each run is pinned to its own cluster via an explicit `-context=$MINIKUBE_PROFILE`
passed to `go test`, so parallel runs never race over kubectl's shared
current-context. (The `e2e-test-upgrade-*` targets shell out to `kubectl`/`helm`
directly and rely on the current-context instead, so avoid running those in
parallel with other profiles from the same shell.)

`e2e-setup-minikube` and `e2e-cleanup-minikube` also accept `MINIKUBE_REUSE`
(default: `false`). With `MINIKUBE_REUSE=true`, setup only starts the profile if
it isn't already running (skipping delete/recreate), and cleanup leaves it
running instead of deleting it. This turns each `MINIKUBE_PROFILE` into a
persistent "shard": running two profiles concurrently every time otherwise means
two `minikube start`/`delete` calls contend for the same host-level locks
(e.g. iptables/docker network setup) on every run, which can make parallel runs
look serialized. With `MINIKUBE_REUSE=true` that cost is paid once, and each
suite's own TestMain teardown still resets Kubernetes-level state between runs:

```bash
make e2e-setup-minikube MINIKUBE_PROFILE=e2e-cluster MINIKUBE_REUSE=true E2E_SETUP_ISTIO=true &
make e2e-setup-minikube MINIKUBE_PROFILE=e2e-namespace MINIKUBE_REUSE=true E2E_SETUP_ISTIO=false &
wait
```

---

## EKS (CI / full test run)

The EKS environment uses a persistent cluster (`jenkins-kube-ninjas`, `us-west-1`).
Worker nodes are scaled to **0** between runs to save cost; the test targets scale
them up and back down automatically.

```bash
# One-time bootstrap (first time only):
bash test/eks-config/setup-eks.sh

# Build and push the operator image, then run all tests:
make e2e-setup-eks
E2E_MARKLOGIC_IMAGE_VERSION=<ecr-image-uri> make e2e-test-eks
make e2e-cleanup-eks
```

For full setup and operational details see [test/eks-config/README.md](eks-config/README.md).

## VAC Core E2E

The opt-in `type=volume-attributes-class` suite runs the same eleven scenarios
in `test/e2e` and `test/e2e-helm`, using shared helpers in
[utils/vac.go](utils/vac.go):

| `case` label | Coverage |
|---|---|
| `without-vac` | Ordinary creation, service configuration, scaling and capacity-only resize without VAC |
| `initial-provisioning` | Parent/child owner references, standalone group, primary/additional classes and admission metadata |
| `primary-live-change` | Parent-driven primary modification without workload/storage replacement |
| `additional-live-change` | Independent additional modification; primary unchanged |
| `inheritance-and-isolation` | Inheritance, whole-object/list overrides, omitted class in a supplied persistence object, and multi-group isolation |
| `scale-after-change` | New ordinal uses the latest class despite historical StatefulSet templates |
| `retained-claim-reuse` | Scale down/up, stable retained claim UIDs and unused-precreated inventory lifecycle |
| `missing-primary-recovery` | Primary-only provisioning/scale fallback and recovery without a CR edit |
| `missing-additional-recovery` | Additional-only fallback and recovery; primary remains independently managed |
| `wrong-driver-rejection` | Initial creation gated; live volumes and service reconciliation remain healthy |
| `pending-binding` | Pending claim identity/reference preserved; latest target applies after scheduling/binding |

**Use a disposable, exclusive cluster.** The existing `TestMain` still installs
and cleans the operator/CRDs/namespaces, even when tests are filtered or VAC is
disabled. Never run these commands against the demo or a shared cluster.
Compile-only checks use `go test -c`, not `-run '^$'` or `-list`.

Provide an existing expandable, `WaitForFirstConsumer` StorageClass and two
valid, matching-driver VACs with different parameters. Do not use the ordinary
Minikube provisioner as evidence of CSI VAC support. EBS cases require complete
gp3 profiles (`type`, `iops`, `throughput`), AWS CLI, explicit region, and
credentials permitting `ec2:DescribeVolumes`. The suite checks actual EBS
parameters as well as native applied class and current-generation parent/child
status. Other CSI drivers receive native-state checks, not provider-specific
parameter qualification. Profiles and StorageClasses supplied by the caller
are never changed. The test principal needs create/delete permissions for its
uniquely named missing/wrong-driver VAC fixtures.

After the normal isolated-cluster setup, supply the candidate operator image
through the existing harness settings (`IMG` for cluster installation or
`E2E_DOCKER_IMAGE` for Helm). The kubeconfig must select the same disposable
context for the harness's external commands as the `--context` argument:

```bash
export KUBECONFIG='/path/to/disposable-cluster-kubeconfig'
export KUBE_CONTEXT='<disposable-context>'
export E2E_MARKLOGIC_IMAGE_VERSION='<approved-MarkLogic-image>'
export E2E_VAC_ENABLED=true
export E2E_VAC_STORAGE_CLASS='<expandable-wffc-storageclass>'
export E2E_VAC_STANDARD_CLASS='<standard-vac>'
export E2E_VAC_PERFORMANCE_CLASS='<performance-vac>'
export AWS_REGION='<cluster-region>'

# Run one installation scope at a time. These commands execute the e2e harness.
go test -v -count=1 -timeout 6h ./test/e2e-helm \
	-run '^TestVolumeAttributesClassNamespaceScoped$' \
	-args --context="$KUBE_CONTEXT" --labels='type=volume-attributes-class'

# Alternative: cluster-scoped installation, not a second overlapping operator.
go test -v -count=1 -timeout 6h ./test/e2e \
	-run '^TestVolumeAttributesClassClusterScoped$' \
	-args --context="$KUBE_CONTEXT" --labels='type=volume-attributes-class'
```

Helm's default watch list includes `ml-ns-vac`. An explicit
`E2E_WATCHED_NAMESPACES` override must include it before installation. Cluster
mode uses `ml-vac`. To focus one scenario, add `,case=primary-live-change` to
the label expression. `E2E_VAC_SIZE` defaults to `20Gi` (choose a size supporting
the requested provider performance), and
`E2E_VAC_TIMEOUT` defaults to `30m` per convergence wait. Cases run serially
with fresh uniquely named claims to avoid EBS modification-quota coupling;
reserve capacity for at least three MarkLogic Pods and six volumes at peak.

VAC-only changes check unchanged Pod/StatefulSet/PVC/PV identities, templates,
restart counts and capacity, plus authenticated document write/read and marker
files on both mounts. The resize regression deliberately permits its existing
restart/template-sync workflow. Pending-binding recovery restarts only its
intentionally unscheduled consumer after removing the selector (`OnDelete`).
The suite does not fake Kubernetes-owned PVC status or interrupt shared CSI.

Each case deletes its own CRs, waits for workloads to stop, then deletes only
recorded/uniquely marked PVCs with UID preconditions and its own VAC fixtures.
`Retain` reclaim policies still leave billable PVs/EBS volumes requiring manual
inventory and cleanup. `E2E_VAC_KEEP_RESOURCES=true` skips per-case cleanup,
but does **not** disable the enclosing harness teardown. Failures dump status,
Pods and Events without auth fields. Opt-out, restart-during-modification,
resize/VAC overlap, provider rejection, API-absent/beta-only and permission-fault
lanes remain separate qualification work, not passes inferred from this suite.

---

## Run Specific Test Types
Each test is assigned a "type" label, allowing you to run only the tests of a specified type.

```bash
# Cluster-scoped (make e2e-test now defaults to namespace-scoped — see below)
make e2e-test-cluster
```

> **Note:** Namespace-scoped behaviour is **not** validated by `test/e2e`.
> The `test/e2e` suite always deploys the operator via `make deploy`
> (kustomize, ClusterRole/ClusterRoleBinding, secure metrics on `:8443`)
> and does not patch `WATCH_NAMESPACE` or metrics flags at runtime.
> To validate namespace-scoped RBAC and the insecure metrics endpoint,
> use the Helm-based suite below: `make e2e-test-helm-namespace`.

### Run specific test types

Use Make targets when possible so the repository defaults for image/tooling variables are applied consistently.

```bash
# Dynamic host lifecycle (recommended)
make e2e-test-dynamic-host

# Dynamic host lifecycle against local code changes (build + minikube image load)
make e2e-test-dynamic-host-local

# Optional: override the local test image tag
make e2e-test-dynamic-host-local LOCAL_E2E_IMG=my-operator:e2e-local

# Volume resize (recommended)
make e2e-test-volume-resize
```

When testing newly added operator behavior (such as dynamic-host reconciliation), prefer `make e2e-test-dynamic-host-local`. This target now builds and loads a dedicated local tag (`marklogic-operator-kubernetes:e2e-local` by default), which avoids accidentally reusing a stale released image such as `progressofficial/marklogic-operator-kubernetes:1.2.0`.

If you need direct label filtering, each test is assigned a `type` label:

```bash
# TLS named cert
go test -v ./test/e2e -count=1 -args --labels="type=tls-named-cert"

# TLS self-signed cert
go test -v ./test/e2e -count=1 -args --labels="type=tls-self-signed"

# HAProxy path-based routing
go test -v ./test/e2e -count=1 -args --labels="type=haproxy-pathbased-enabled"

# Metrics endpoint
go test -v ./test/e2e -count=1 -args --labels="type=metrics"
```

---

## test/e2e-helm — Namespace-scoped (Helm, default)

This suite installs the operator via the Helm chart with `scope.type=namespace` so the operator
runs with only a Role/RoleBinding per watched namespace — no ClusterRole backstop.
It is the only reliable way to validate namespace-scoped RBAC behaviour, and is what
`make e2e-test` runs by default.

### Setup and run

```
make e2e-setup-minikube
make e2e-test-helm-namespace
make e2e-cleanup-minikube
```

Or run directly (requires a running cluster with `helm` in PATH):

```
go test -v -count=1 -timeout 45m ./test/e2e-helm
```

### Environment variables

| Variable | Default | Description |
|---|---|---|
| `E2E_DOCKER_IMAGE` | _(none)_ | Operator image (`repo:tag`). If set, loaded into minikube with `pullPolicy=Never`. |
| `E2E_MARKLOGIC_IMAGE_VERSION` | _(none)_ | MarkLogic server image used in all CRs. |

### Watched namespaces

The Helm chart is installed with:

```
scope.watchNamespaces=ml-ns-test,ml-ns-ednode,ml-ns-tls,ml-ns-tls-named,ml-ns-tls-ednode,ml-ns-haproxy-path,ml-ns-haproxy,ml-ns-log
```

All test namespaces must appear in this list. The operator has no RBAC outside these namespaces.

### Run specific test types

```bash
# RBAC validation (ClusterRole absent, Role/RoleBinding present per namespace)
go test -v ./test/e2e-helm -count=1 -args --labels="type=rbac"

# Namespace-scoped cluster reconciliation
go test -v ./test/e2e-helm -count=1 -args --labels="type=cluster-ns"

# dnode + enode two-group cluster
go test -v ./test/e2e-helm -count=1 -args --labels="type=ednode"

# TLS self-signed
go test -v ./test/e2e-helm -count=1 -args --labels="type=tls-self-signed"

# Metrics endpoint (insecure HTTP, no auth)
go test -v ./test/e2e-helm -count=1 -args --labels="type=metrics"
```
