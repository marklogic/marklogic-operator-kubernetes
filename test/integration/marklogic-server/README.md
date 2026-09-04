# MarkLogic Server Integration Tests

Integration tests that exercise the operator against a **real MarkLogic cluster**
running in Kubernetes, rather than mocking the Kubernetes API (as the unit tests
under `internal/controller` do). These tests provision actual `MarklogicCluster`
resources, wait for the operator to reconcile them, and then talk to the live
MarkLogic server(s) — optionally alongside supporting infrastructure such as
Keycloak and HAProxy — to validate end-to-end behavior.

> These are **opt-in** tests, gated behind environment variables so they don't run
> as part of the normal fast unit-test pass.

## Prerequisites

- A running Kubernetes cluster (minikube, kind, EKS, ...) selected by the current
  `kubectl` context, with a default `StorageClass` that can provision
  `PersistentVolumeClaim`s.
- The MarkLogic operator installed in the cluster (the tests create
  `MarklogicCluster` resources reconciled by the operator).
- Go toolchain and access to pull the MarkLogic (and, for OAuth tests, Keycloak,
  HAProxy, curl) images used by the fixtures.

## Directory layout

| Path | Purpose |
| --- | --- |
| [`oauth/`](oauth/README.md) | OAuth 2.0 (Authorization Code and Resource Server/JWT bearer) integration tests behind the operator-managed HAProxy load balancer, plus the HAProxy SessionID affinity contract test. See [oauth/README.md](oauth/README.md) for details. |
| `fixtures/` | Reusable builders for Kubernetes objects used by the integration tests. |
| `fixtures/marklogiccluster/` | Builds a `MarklogicCluster` custom resource (TLS-enabled, HAProxy-fronted, two-node) for a test namespace. |
| `fixtures/keycloak/` | Builds the Keycloak `Deployment`/`Service` fixture and realm import used for OAuth tests. |
| `fixtures/oauthclient/` | Builds an idle `curl` client `Pod` (with cookie jar and CA mounted) used to drive HTTP flows against MarkLogic/Keycloak from inside the cluster. |
| `fixtures/tls/` | Generates a self-signed CA and server certificate/key, and builds the corresponding `Secret`s for TLS-enabled tests. |
| `testutil/` | Shared test helpers: a Kubernetes scheme for decoding operator/core types, `kubectl`-based diagnostics collection, etc. |

## Running the tests

Each test suite is gated behind its own environment variable(s) — see the
suite's own README (currently [oauth/README.md](oauth/README.md)) for the exact
variables, namespaces, and `go test -run` invocations.

General pattern:

```sh
<GATE_ENV_VAR>=true \
MARKLOGIC_IMAGE=<marklogic-image> \
go test -v -count=1 -timeout 45m \
  -run <TestName> \
  ./test/integration/marklogic-server/<suite>
```

## Cleanup

Tests default to deleting their namespace on completion. Most suites support a
`*_RETAIN_NAMESPACE=true` variable to keep the namespace around for inspection;
remove it manually afterwards with:

```sh
kubectl delete ns <namespace>
```

## Diagnostics on failure

`testutil.CollectKubernetesDiagnostics` is invoked on test failure to log pod,
service, and event state plus container logs for the test namespace, which shows
up directly in the `go test -v` output.
