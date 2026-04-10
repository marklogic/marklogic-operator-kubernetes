# Test MarkLogic Kubernetes Operator with e2e-framework

## Test suites

| Package | Description |
|---|---|
| `test/e2e` | Cluster-scoped tests. Operator deployed via `make deploy` (kustomize, ClusterRole/ClusterRoleBinding). |
| `test/e2e-helm` | Namespace-scoped tests. Operator installed via `helm install` with `scope.type=namespace`. Validates that Role/RoleBinding per watched namespace is sufficient and no ClusterRole is present. |

---

## test/e2e — Cluster-scoped

### Setup and run

```
make e2e-setup-minikube
make e2e-test
make e2e-cleanup-minikube
```

### Run in specific scope mode

```bash
# Cluster-scoped (default)
make e2e-test-cluster

# Namespace-scoped (patches WATCH_NAMESPACE at runtime; metrics.secure forced to false)
make e2e-test-namespace
```

### Run specific test types

Each test is assigned a `type` label, allowing you to run only tests of that type.

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
