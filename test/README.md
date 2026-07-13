# Test MarkLogic Kubernetes Operator with e2e-framework

The operator has two e2e test environments: **Minikube** (local, for development)
and **EKS** (the Jenkins CI target).
## Test suites

| Package | Description |
|---|---|
| `test/e2e` | Cluster-scoped tests. Operator deployed via `make deploy` (kustomize, ClusterRole/ClusterRoleBinding). |
| `test/e2e-helm` | Namespace-scoped tests. Operator installed via `helm install` with `scope.type=namespace`. Validates that Role/RoleBinding per watched namespace is sufficient and no ClusterRole is present. |

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

---

## Run Specific Test Types
Each test is assigned a "type" label, allowing you to run only the tests of a specified type.

```bash
# Cluster-scoped (default — what `make e2e-test` runs)
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

## test/e2e-helm — Namespace-scoped (Helm)

This suite installs the operator via the Helm chart with `scope.type=namespace` so the operator
runs with only a Role/RoleBinding per watched namespace — no ClusterRole backstop.
It is the only reliable way to validate namespace-scoped RBAC behaviour.

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
