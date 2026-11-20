# Test MarkLogic Kubernetes Operator with e2e-framework

The operator has two e2e test environments: **Minikube** (local, for development)
and **EKS** (the Jenkins CI target).

For VolumeAttributesClass (VAC) coverage, see [VAC Core E2E](#vac-core-e2e) and
the administrator guide in [docs/volume-attributes-class.md](../docs/volume-attributes-class.md).
Run the VAC suite only on an exclusive, disposable cluster: the e2e harness
cleans CRDs and namespaces, which is unsafe on shared or demo clusters.

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

The `-context=$MINIKUBE_PROFILE` flag selects the Go test client's cluster.
Suite setup also invokes `kubectl` and `helm`, which use `KUBECONFIG` and its
current context. For parallel profiles, give each process a separate kubeconfig
whose current context matches its profile. The focused object-storage target
below creates this isolated kubeconfig automatically. The other targets,
including `e2e-test-upgrade-*`, still require it to be prepared by the caller.

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

The opt-in `type=volume-attributes-class` suite runs two scenarios in
`test/e2e` and `test/e2e-helm`, using shared helpers in
[utils/vac.go](utils/vac.go):

| `case` label | Coverage |
|---|---|
| `initial-provisioning` | Day-1: parent/child owner references, standalone group, primary/additional classes and admission metadata |
| `primary-live-change` | Day-2: parent-driven primary modification without workload/storage replacement |

**Use a disposable, exclusive cluster.** The existing `TestMain` still installs
and cleans the operator/CRDs/namespaces, even when tests are filtered or VAC is
disabled. Never run these commands against the demo or a shared cluster.
Compile-only checks use `go test -c`, not `-run '^$'` or `-list`.

Provide an existing StorageClass and two valid, matching-driver VACs with
different parameters. Do not use the ordinary
Minikube provisioner as evidence of CSI VAC support. EBS cases require complete
gp3 profiles (`type`, `iops`, `throughput`), AWS CLI, explicit region, and
credentials permitting `ec2:DescribeVolumes`. The suite checks actual EBS
parameters as well as native applied class and current-generation parent/child
status. Other CSI drivers receive native-state checks, not provider-specific
parameter qualification. Profiles and StorageClasses supplied by the caller
are never changed.

After the normal isolated-cluster setup, supply the candidate operator image
through the existing harness settings (`IMG` for cluster installation or
`E2E_DOCKER_IMAGE` for Helm). The kubeconfig must select the same disposable
context for the harness's external commands as the `--context` argument:

```bash
export KUBECONFIG='/path/to/disposable-cluster-kubeconfig'
export KUBE_CONTEXT='<disposable-context>'
export E2E_MARKLOGIC_IMAGE_VERSION='<approved-MarkLogic-image>'
export E2E_VAC_ENABLED=true
export E2E_VAC_STORAGE_CLASS='<storageclass>'
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

In Jenkins, set `VERIFY_VAC=true` with `E2E_RUNTIME=eks` and
`E2E_INSTALL_MODE=fresh`. After the EKS suite, `make e2e-test-eks-vac` applies
the gp3 profiles in [eks-config/vac-profiles.yaml](eks-config/vac-profiles.yaml)
and runs the cluster-scoped VAC suite into `test_results/e2e-eks-vac.xml`. The
EKS cluster must run Kubernetes 1.34+ with the EBS CSI driver; it is off by
default and not part of nightly builds.

`TestVolumeAttributesClassUnsupportedEnvironment` (cluster suite) runs only
when no VAC API is served, such as the default Minikube 1.31 lane, and skips
otherwise. It selects a VAC and checks that the group reports
`StorageAttributesValidated=False/VACUnsupported` with a Warning event, stays
Ready, and provisions its PVC without a class. The upgrade suite starts from
Operator 1.3.1 by default and verifies that a PVC-backed cluster without VAC
keeps its StatefulSet claim templates and PVC UIDs/specs unchanged.

Helm's default watch list includes `ml-ns-vac`. An explicit
`E2E_WATCHED_NAMESPACES` override must include it before installation. Cluster
mode uses `ml-vac`. To focus one scenario, add `,case=primary-live-change` to
the label expression. `E2E_VAC_SIZE` defaults to `20Gi` (choose a size supporting
the requested provider performance), and
`E2E_VAC_TIMEOUT` defaults to `15m` per convergence wait. Cases run serially
with fresh uniquely named claims to avoid EBS modification-quota coupling;
reserve capacity for at least two MarkLogic Pods and four volumes at peak.

VAC-only changes check unchanged Pod/StatefulSet/PVC/PV identities, templates,
restart counts and capacity, plus authenticated document write/read and marker
files on both mounts. The suite does not fake Kubernetes-owned PVC status or
interrupt shared CSI.

Each case deletes its own CRs, waits for workloads to stop, then deletes only
recorded/uniquely marked PVCs with UID preconditions.
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
scope.watchNamespaces=ml-ns-test,ml-ns-ednode,ml-ns-tls,ml-ns-tls-named,ml-ns-tls-ednode,ml-ns-haproxy-path,ml-ns-haproxy,ml-ns-log,ml-ns-resize-a,ml-ns-resize-b,ml-ns-vac,ml-ns-object-storage
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

## Object storage credentials

Unit and envtest coverage for `spec.objectStorage` does not require a live
MarkLogic instance or cloud account:

```sh
# Unit: credential client, provider lifecycle/checkpoints, handler isolation, predicates, freshness helper
env -u ML_MANAGE_ENDPOINT go test ./api/... ./pkg/... ./test/utils/... -run 'ObjectStorage|Credential|ReconcileOutcome|ClusterHandler|ClusterAnnotations|BootstrapManagement' -count=1
env -u ML_MANAGE_ENDPOINT go test ./internal/controller -run 'TestClusterPredicate|TestSecretRevision|TestObjectStorageSecretNames|TestSecretToClusters' -count=1

# envtest (schema/CEL, status round trip, real Secret and cluster watches); needs an absolute KUBEBUILDER_ASSETS
export KUBEBUILDER_ASSETS="$PWD/$(bin/setup-envtest use 1.36.2 --bin-dir bin -p path)"
go test ./internal/controller -run TestAPIs -ginkgo.focus "object storage" -count=1
```

Use `test/utils.ObjectStorageReady` for any readiness wait: it checks all four status-freshness conditions (a phase-only `Applied` check is not enough) from the current cluster and Secret metadata. Never print Secret data or raw credential responses from tests.

### Focused E2E runs

`TestObjectStorageNamespaceScoped` and `TestObjectStorageClusterScoped` share
the lifecycle checks in `test/objectstorage`. Both have the label
`type=object-storage`. They use `ml-ns-object-storage` and `ml-object-storage`,
respectively, with unique cluster, group, and Secret names per run.

Use a disposable Kubernetes cluster. The surrounding E2E suite installs and
removes the operator and resets MarkLogic CRDs, so it must not run against a
shared deployment. The focused test cleans up the resources it creates and
preserves a namespace that already existed.

```sh
# Prepare the disposable profile and the server image first.
make e2e-setup-minikube MINIKUBE_PROFILE=object-storage-test E2E_SETUP_ISTIO=false

# Always build this checkout and load its operator image before testing.
# The default scope is namespace (Helm; workload permissions are namespaced).
make e2e-test-object-storage-local MINIKUBE_PROFILE=object-storage-test
make e2e-test-object-storage-local MINIKUBE_PROFILE=object-storage-test E2E_SCOPE=cluster

# If the candidate image is already available on every target node:
make e2e-test-object-storage E2E_CONTEXT=object-storage-test IMG=my-operator:object-storage-test
```

`E2E_CONTEXT` defaults to `MINIKUBE_PROFILE`. The focused target copies only
that context into a temporary mode-0600 kubeconfig and exports it to the Go
test process and all `kubectl`/`helm` subprocesses, then removes the file on
exit. It does not change the shared kubeconfig's current context. The local
target always builds and loads `LOCAL_E2E_IMG` (default
`marklogic-operator-kubernetes:e2e-local`) and forces `E2E_CONTEXT` to the
selected `MINIKUBE_PROFILE`; an existing image tag never skips the build.
The Helm suite uses `imagePullPolicy: Never` for an explicitly selected image,
so the non-local target requires that image to be available on its nodes.

The object-storage test is deliberately sequential: restart cases scale the
shared operator Deployment to zero and restore its original replica count.
Do not run another suite against that installation concurrently. Workload Pod
UIDs, templates, running container IDs, image IDs, start times and restart counts
must remain unchanged across the controller restarts and lifecycle operations.

### Credential fixtures and skipped coverage

Missing-Secret and invalid-key checks run without cloud credentials. To run
the successful application and credential lifecycle variants as well, set
`E2E_OBJECT_STORAGE_FIXTURE` to an **absolute path** to a private JSON file
outside the repository. Supply both AWS and Azure initial and rotated
credential material:

```json
{
  "aws": {
    "initial": {"accessKey": "...", "secretKey": "...", "sessionToken": "optional"},
    "rotated": {"accessKey": "...", "secretKey": "..."}
  },
  "azure": {
    "initial": {"storageAccount": "...", "storageKey": "..."},
    "rotated": {"storageAccount": "...", "storageKey": "..."}
  }
}
```

The strings above are placeholders. Use dedicated test credentials, make
the file readable only by its owner, and inject its path through the test
environment rather than putting credential values in commands or reports.
AWS `sessionToken` is optional in either revision. Rotation material must
differ from the corresponding initial material. Do not check the fixture
into source control.
Duplicate JSON fields, case aliases and non-string values (including `null`
tokens) are rejected rather than silently changing which material is tested.

For token-clear coverage, `aws.initial` must contain valid temporary credentials
with a nonempty token, and `aws.rotated` must contain valid static credentials
with no token (or an empty one). Other valid fixture combinations still run the
lifecycle but explicitly skip token clearing. Each `absent_key` / `empty_value`
variant first re-applies the temporary credentials and privately checks the
stored token, then applies static credentials and checks that it is absent or
empty. The verifier uses a separate generated user with `manage` plus the
`credentials-get-aws` privilege. Authentication and request bodies travel over
exec stdin; bounded responses stay in memory. No credential GET body is logged,
and the harness never deletes provider credentials. Unknown JSON response
shapes fail the test instead of being interpreted as an empty token.

```sh
E2E_OBJECT_STORAGE_FIXTURE=/absolute/private/object-storage.json \
  make e2e-test-object-storage-local MINIKUBE_PROFILE=object-storage-test
```

Without the fixture, the credential lifecycle subtest explicitly reports
`SKIP`; this is not live lifecycle evidence. A top-level Go test can pass
while one of its subtests skips. The suite banner counts top-level tests;
use verbose subtest output or `go test -json` (for example, set
`GO_TEST='go test -json'` on the Make target) to retain each variant's result.

### Coverage and remaining release evidence

The focused E2E suite supplies only the variants described below; passing
it does not complete every row in the [spec test matrix](../docs/spec/Cloud%20Storage%20Credential.md#test-matrix).

The names below are subtests under each scoped entry point. All rows except
the namespace-isolation, missing-Secret, scaling, and safety checks are nested under
`credential_lifecycle` and require the fixture.

| Spec IDs | Subtest | Evidence and limits |
|---|---|---|
| OS-T05 | `OS-T05_cross_namespace_secret_does_not_resolve` | A same-name Secret exists only in a separately owned namespace. A generation change forces fresh evaluation; both providers must still report local `SecretNotFound` with no observed UID/revision. No credentials are required. |
| OS-T02, OS-T05 | `OS-T05_missing_secret_and_revision_watch` | Both providers fail on a missing Secret, observe creation with missing required keys, and advance observed metadata on an annotation-only revision. The complete normalization and predicate matrices remain unit/envtest coverage. |
| OS-T11 | `OS-T11_scale_while_credentials_invalid` | The cluster scales from one to two ready pods while both providers report `SecretKeyMissing`. Other core-step and status-write failures require separate evidence. |
| OS-T03 | `OS-T03_initial_apply_separate_secrets` | Both providers reach fresh `Applied` using separate Secrets. This starts after pod readiness; it does not cover the initial `Pending/BootstrapNotReady` transition. |
| OS-T07, OS-T13 | `OS-T07_restart_applied_and_detached` | Stops/restarts the real operator with both providers Applied, then with both Detached. Secret revisions and a CR-generation change prove the new manager processed the cluster; success/history and workload identities survive, and identical Secret recreation resumes management. |
| OS-T04 | `OS-T04_metadata_unrelated_keys_and_region` | Annotation-only and unrelated-key Secret changes, plus an AWS region edit, preserve the success record while readiness remains fresh. It does not count remote PUTs. |
| OS-T05 | `OS-T05_manual_reconcile_preserves_children` | A reconcile-request annotation plus a Secret-revision processing barrier preserve group annotations, the StatefulSet pod template, and pod UIDs. This does not isolate which watch caused reconciliation or exercise corrected privileges. |
| OS-T03, OS-T08 | `OS-T03_independent_rotation` | Each provider's changed material establishes a fresh source checkpoint while the other's success remains unchanged. This does not prove cloud authorization or old-credential rejection. |
| OS-T02 | `OS-T02_session_token_clearing/absent_key`, `empty_value` | Fresh temporary-to-static applications followed by private real Management API reads verify both token-clearing forms. Requires the token fixture described above; token-only fingerprint sensitivity is covered separately by unit tests. |
| OS-T07 | `OS-T07_detach_and_identical_recreation` | Each eligible Secret deletion produces `Detached` with an expiry warning; identical material recreated under a new UID establishes fresh success. Bootstrap outages and injected read failures remain separate. |
| OS-T08 | `OS-T08_shared_secret_independent_eligibility` | Invalidating each provider in a shared Secret preserves the other provider; deletion produces `Failed` versus `Detached`, then recreation restores both. |
| OS-T13 | `OS-T13_incomplete_success_requires_new_checkpoint` | Stops the controller, seeds incomplete historical success for both providers, and proves readiness false while generation and Secret revisions are still current. It then advances metadata, restarts, and requires fresh complete checkpoints for unchanged material. The incomplete state is injected; this does not time a crash at an actual PUT boundary. |
| OS-T12 | `OS-T12_remove_providers` | Removing one provider preserves the other; removing both clears status. Remote non-revocation and controller-attributed no-GET/no-DELETE evidence remain outstanding. |
| OS-T14 | `OS-T14_sanitized_status_events_and_operator_logs` | Checks status, events, and logs for known fixture/generated credentials, failing when the client-side capture limit is exceeded. Intentional restarts scan each old log source before stopping and establish a new verified container instance afterward; observed startup crashes, replacements, and restarts fail. Already-rotated logs and shutdown output after the pre-stop capture need retained cluster logs for a complete audit. |

### Object storage installation upgrades

`test/objectstorage-upgrade` has a separate runner without the ordinary suite's
destructive `TestMain` reset. It requires a clean disposable cluster: existing
MarkLogic CRDs or installation resource identities cause it to stop before
installation. It uses source and candidate artifact directories plus explicit
source/candidate image tags; it does not fetch releases or build images.
The first candidate must also have different running image content from the
source: distinct tags resolving to the same image ID do not count as an upgrade.

```sh
# Both artifact roots must contain charts/marklogic-operator-kubernetes and
# config/default as applicable. Make the images available to the cluster first.
export E2E_OBJECT_STORAGE_UPGRADE_SOURCE_DIR=/absolute/source-release
export E2E_OBJECT_STORAGE_UPGRADE_SOURCE_IMAGE=registry/operator:source-version
export E2E_OBJECT_STORAGE_FIXTURE=/absolute/private/object-storage.json

# Candidate artifacts default to this checkout; IMG selects the candidate image.
make e2e-test-object-storage-upgrade E2E_CONTEXT=object-storage-test \
  E2E_SCOPE=namespace IMG=registry/operator:candidate-version
make e2e-test-object-storage-upgrade E2E_CONTEXT=object-storage-test \
  E2E_SCOPE=cluster IMG=registry/operator:candidate-version
```

The namespace variant uses source/candidate Helm charts; the cluster variant
uses their Kustomize resources with its workload in a different namespace from
the operator to exercise cluster-wide access. The baseline workload stays in place throughout.
The old controller stops before CRD replacement; the candidate stays at zero
replicas while the expanded spec/status schema is round-tripped through the
API server, including all four phases and explicit false eligibility. The actual
held package installation must preserve the existing checkpoint and pass another
schema roundtrip. It starts only after those checks. Cluster, group, StatefulSet,
server Pod and running container identities must survive.

When the source supports durable checkpoints, the source controller establishes
real AWS `Applied` and Azure `Detached` states. The candidate preserves unchanged
success; a fingerprint-format change may establish a fresh AWS checkpoint as
allowed by the spec. Azure's absent-Secret history must survive, followed by
re-adoption of its recreated Secret. With an older schema, the runner injects
explicitly synthetic legacy `Applied` records while both controllers are stopped,
proves they are not ready, then requires complete fresh application by the candidate.
That branch tests migration recovery, not prior successful credential application.
Its synthetic fingerprint also changes. Both upgrade branches then capture real
candidate-published fingerprints, stop the candidate, remove completeness fields,
and prove readiness false with unchanged current Secret markers. Restart must
establish complete checkpoints with those same fingerprints; subsequent unchanged
reconciliation must preserve success. Fingerprints are never recomputed by tests.
Namespace scope permits the chart's narrow StorageClass reader ClusterRole; the
operator's Secret and MarkLogic workload permissions must stay namespaced.

Direct package execution requires `E2E_OBJECT_STORAGE_UPGRADE_RUN=true`,
`E2E_OBJECT_STORAGE_UPGRADE_SCOPE`, `E2E_OBJECT_STORAGE_UPGRADE_CONTEXT`, absolute
`E2E_OBJECT_STORAGE_UPGRADE_SOURCE_DIR` / `E2E_OBJECT_STORAGE_UPGRADE_TARGET_DIR`,
distinct `E2E_OBJECT_STORAGE_UPGRADE_SOURCE_IMAGE` / `E2E_OBJECT_STORAGE_UPGRADE_TARGET_IMAGE`,
`E2E_MARKLOGIC_IMAGE_VERSION`, and `E2E_OBJECT_STORAGE_FIXTURE`. The Make target
sets the opt-in, selected scope/context and candidate defaults. With opt-in unset,
the standalone tests skip without contacting Kubernetes.

Compile/local validation is safe without a cluster:

```sh
go test ./test/objectstorage -count=1
go test -c -o /tmp/object-storage-upgrade.test ./test/objectstorage-upgrade
go test -c -o /tmp/object-storage-cluster.test ./test/e2e
go test -c -o /tmp/object-storage-helm.test ./test/e2e-helm
```

Additional spec variants remain outside these E2E cases: full strict/non-strict
declaration validation through each packaged CRD (OS-T01), initial
`Pending/BootstrapNotReady` before successful startup (OS-T03), privilege correction
with an annotation-only retry (OS-T05), detachment during bootstrap outages or
Secret read denial (OS-T07), and isolation from injected unrelated core-step
failures (OS-T11). Existing unit/envtest coverage is separate evidence and does
not turn these missing E2E variants into passes. No live PUT-count or no-op status-write
assertion is inferred merely from an unchanged success checkpoint.

The restart, upgrade and token-clear cases are implemented, but live results for
each source/candidate version and scope must still be recorded. No standalone
live Management API failure-matrix runner is wired up. Cloud object probes and
rotation with old-credential rejection (OS-T15), and access-log attribution for
controller credential requests (OS-T12), remain gaps. Missing or unattributable access logs leave
the OS-T12 request-absence assertion **BLOCKED**, not passed. A successful
Management API application is not proof of cloud authorization or rotation.
