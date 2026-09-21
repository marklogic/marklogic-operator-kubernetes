# MarkLogic Server Integration Tests

Opt-in tests against existing Kubernetes clusters, including MarkLogic product
scenarios and small platform contract examples. MarkLogic scenarios provision
`MarklogicCluster` resources and exercise live servers alongside fixtures such as
Keycloak and HAProxy. The HAProxy contract and platform teaching example do not
require MarkLogic. Controller tests under `internal/controller` instead use
local envtest API servers and etcd.

Start with the [integration entry point](../README.md) to discover scenarios,
run local checks, or add a new suite.

> These are **opt-in** tests, gated behind environment variables so they don't run
> as part of the normal fast unit-test pass.

## Prerequisites

These requirements apply to MarkLogic scenarios. Use
`make integration-describe SCENARIO=<name>` for scenario-specific prerequisites.

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
| [`platform/`](platform/README.md) | Runnable ConfigMap teaching example; no OAuth or MarkLogic dependency. |
| [`../scenarios/catalog.json`](../scenarios/catalog.json) | Names, package/test selections, gates, and prerequisites consumed by the runner. |
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

From the repository root, discover and check scenarios without cluster access:

```sh
make integration-list
make integration-describe SCENARIO=oauth-resource-server
make integration-check
make integration-test-local
```

Run a selected scenario against an existing cluster:

```sh
INTEGRATION_CONTEXT=<context> \
INTEGRATION_OPERATOR_NAMESPACE=<operator-namespace> \
INTEGRATION_OPERATOR_DEPLOYMENT=<operator-deployment> \
MARKLOGIC_IMAGE=<image-under-test> \
make integration-test SCENARIO=oauth-resource-server
```

The catalog includes `oauth-resource-server`, `oauth-authorization-code`,
`haproxy-session-affinity`, and the non-OAuth `platform-smoke` teaching example. The affinity contract uses nginx and does not require
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
| `INTEGRATION_RESULTS_DIR` | Optional results root; defaults to `test/test_results/integration/` at the repository root. Every run creates a unique child directory. |
| `INTEGRATION_TIMEOUT` | Overall Go test timeout, default `45m`; allow time for workload startup and cleanup. |
| `INTEGRATION_RETAIN_NAMESPACE` | Set to `true` to retain the namespace after any result. The older `MARKLOGIC_OAUTH_RETAIN_NAMESPACE` remains supported. |

The runner selects exactly one live suite and disables inherited gates for the
others. It validates the exact package/test selection and checks actual execution
through Go JSON events, rejecting missing/skipped targets and failed processes.
It reports the source commit and whether the working tree has changes.
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

Run local helper tests with all registered live gates explicitly disabled:

```sh
make integration-test-local
```

Direct `go test ./test/integration/...` is also available when live gates are
unset. Prefer the make target if your shell may inherit enabled gates.

## Isolation, cleanup, and diagnostics

Every enabled suite creates a fresh namespace with a scenario prefix and a run
ownership label. The same label is added to resources submitted by the fixture;
operator-created children remain contained in that namespace. Namespaces are
never adopted from an earlier run.

On failure, diagnostics save selected pod status fields, events, and container
logs in the run directory. Cleanup verifies namespace ownership and UID, uses
deletion preconditions, and waits up
to five minutes for namespace and bound PV deletion. A timeout or unsafe cleanup
condition fails the test. Provider-side disk deletion is not independently
verified by this helper.

Use `INTEGRATION_RETAIN_NAMESPACE=true` to keep an environment. The test prints
the generated namespace and commands for inspection and manual cleanup using the
same context and kubeconfig. Do not use a fixed namespace from an earlier run.

## Results and diagnostics

Each enabled live suite prints its unique results directory before preflight.
Directories use mode `0700`, files use `0600`, and the default results root is
ignored by Git. Custom results roots should also be outside tracked source files.

| File | Contents |
| --- | --- |
| `run.json` | Schema version, run ID, scenario/test, context/server, commit and working-tree state, namespace, stage timestamps, component image references/IDs, applied resource names/kinds, cases, outcome, and cleanup status. Updated atomically as the run progresses. |
| `junit.xml` | Local JUnit export of subtest results, plus a lifecycle failure when setup or cleanup fails. Unmet prerequisites count as a failure rather than a passing skip. |
| `diagnostics.json` | On failure: selected pod/container status, events, log filenames, and collection errors. |
| `<pod>-<container>.log` | On failure: up to 200 lines / 64 KiB of current logs. |
| `<pod>-<container>-previous.log` | On failure: previous logs for containers with a nonzero restart count. |

`run.json` distinguishes `running`, `passed`, `failed`, `skipped`, and
`prerequisites_unmet`. A process killed before finalization leaves `running` and
no finish timestamp; do not interpret that as success. A failed cleanup fails the
suite and records `cleanup: failed`. Retained environments record `retained`.
Subtests registered through `run.Case` are included individually, including skips.

The normal unit-test pass skips disabled live suites before creating reports.
Likewise, runner argument/tool validation can fail before the Go suite starts;
those failures are reported by the command exit status and do not create run
artifacts. Reports do not replace checking the runner's exit status.

Diagnostics have a two-minute total budget and a 20-second limit per log request.
They omit full manifests, annotations, pod arguments and environment values.
Fixture credentials discovered in Secret data, authentication fields, and embedded
JSON are redacted from collected logs, along with common token, cookie, JWT,
private-key and URL credential formats. This is best-effort filtering: new log
formats may require additional rules. Raw apply/exec failure output is omitted
because it can echo manifests or authentication responses. Arbitrary stdout from
new scenario code is not automatically sanitized; contributors must avoid logging
credentials themselves.

For the meeting demo, use the printed directory and `run.json` to show the
scenario, stages, assertions, and cleanup outcome. Keep a redacted result from a
rehearsal as a fallback if the live environment is unavailable. `junit.xml` is an
additional local export; CI integration is outside the current demo scope.
