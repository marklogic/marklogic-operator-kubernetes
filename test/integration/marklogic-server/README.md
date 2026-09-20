# MarkLogic Server Integration Tests

Integration tests that exercise the operator against a **real MarkLogic cluster**
running in Kubernetes. Controller tests under `internal/controller` use envtest
with a local Kubernetes API server and etcd; they do not deploy MarkLogic workloads. These tests provision actual `MarklogicCluster`
resources, wait for the operator to reconcile them, and then talk to the live
MarkLogic server(s) — optionally alongside supporting infrastructure such as
Keycloak and HAProxy — to validate end-to-end behavior.

> These are **opt-in** tests, gated behind environment variables so they don't run
> as part of the normal fast unit-test pass.

## Prerequisites

- A running Kubernetes cluster (minikube, kind, EKS, ...) selected explicitly with
  `INTEGRATION_CONTEXT`, with a `StorageClass` that can provision
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

## Contributing

See the [contribution guide](../CONTRIBUTING.md) and
[scenario template](../SCENARIO_TEMPLATE.md) for requirements, ownership, layout,
and review expectations.

## Running the tests

From the repository root:

```sh
INTEGRATION_CONTEXT=<context> \
INTEGRATION_OPERATOR_NAMESPACE=<operator-namespace> \
INTEGRATION_OPERATOR_DEPLOYMENT=<operator-deployment> \
MARKLOGIC_IMAGE=<image-under-test> \
make integration-test SCENARIO=oauth-resource-server
```

Available scenarios are `oauth-resource-server`, `oauth-authorization-code`, and
`haproxy-session-affinity`. The affinity contract uses nginx and does not require
a MarkLogic image, operator, or storage class. Authorization Code requires a
12.1+ image and a matching `MARKLOGIC_VERSION`, for example `12.1.0`. That value is
a caller declaration, not proof of the version inside a custom image.

| Setting | Purpose |
| --- | --- |
| `INTEGRATION_CONTEXT` | Required explicit context; never changes the shared current-context. |
| `KUBECONFIG` | Optional standard Kubernetes config file(s), used by both client-go and kubectl. |
| `INTEGRATION_OPERATOR_NAMESPACE` / `INTEGRATION_OPERATOR_DEPLOYMENT` | Identify the operator to check for MarkLogic scenarios; it must watch newly created test namespaces. |
| `MARKLOGIC_IMAGE` | Required by the runner for MarkLogic scenarios; supporting images remain pinned in fixtures. |
| `MARKLOGIC_VERSION` | Required version declaration for Authorization Code through the runner. |
| `INTEGRATION_STORAGE_CLASS` | Select a class explicitly; otherwise require exactly one default class. It must use Delete reclaim policy. |
| `INTEGRATION_TIMEOUT` | Overall Go test timeout, default `45m`; allow time for workload startup and cleanup. |
| `INTEGRATION_RETAIN_NAMESPACE` | Set to `true` to retain the namespace after any result. The older `MARKLOGIC_OAUTH_RETAIN_NAMESPACE` remains supported. |

The runner selects exactly one live suite and disables inherited gates for the
others. It reports the source commit and whether the working tree has changes.
Preflight checks connectivity, test-user permissions, served MarkLogic CRDs,
operator availability, and storage selection before workload deployment. The
current preflight requires permissions across test namespaces, because namespaces
are generated for each run. It does not grant permissions or install an operator.

Preflight cannot establish image pull access, available node capacity, operator
watch scope, or header compatibility from the operator image tag. The deployed
scenario remains the validation of those behaviors. See the OAuth README for the
required HAProxy header fix. A successful preflight is not a successful live test.

Direct `go test` with the suite's opt-in gate remains available, but also requires
`INTEGRATION_CONTEXT` and the operator settings for MarkLogic scenarios. Prefer
the runner for consistent selection and version declarations.

Run local helper tests without live gates:

```sh
go test ./test/integration/marklogic-server/...
```

## Isolation, cleanup, and diagnostics

Every enabled suite creates a fresh namespace with a scenario prefix and a run
ownership label. The same label is added to resources submitted by the fixture;
operator-created children remain contained in that namespace. Namespaces are
never adopted from an earlier run.

On failure, diagnostics log pod, service, event, and container state. Cleanup
verifies namespace ownership and UID, uses deletion preconditions, and waits up
to five minutes for namespace and bound PV deletion. A timeout or unsafe cleanup
condition fails the test. Provider-side disk deletion is not independently
verified by this helper.

Use `INTEGRATION_RETAIN_NAMESPACE=true` to keep an environment. The test prints
the generated namespace and commands for inspection and manual cleanup using the
same context and kubeconfig. Do not use a fixed namespace from an earlier run.
