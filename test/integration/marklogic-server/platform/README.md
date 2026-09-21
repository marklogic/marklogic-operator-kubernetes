# Platform ConfigMap example

Status: Implemented; local contracts verified, live cluster validation pending.
This is a runnable teaching example for contributors, not a PDC test or evidence
of MarkLogic/operator behavior. Requirements owner and long-term maintainer: TBD
with the team before adopting it as a maintained product scenario.

## Behavior and scope

`TestPlatformConfigMapLifecycle` creates a fresh owned namespace, applies a
ConfigMap and one small BusyBox Pod, waits for readiness, and records two cases:

| Case | Required result | Failures |
| --- | --- | --- |
| `mounted_value` | `/config/message` contains the exact fixture value. | Exec/read errors, wrong or missing content. |
| `absent_key` | An undeclared key has no file in the ConfigMap mount. | Exec errors or an unexpected file. |

Setup/readiness/cleanup failures fail the suite. It reuses `testutil.NewRun`,
`run.ApplyObjects`, `run.Stage`, `run.Case`, and `run.LogImages`; no OAuth setup is
imported or copied. The builder stays local to this suite until another scenario
needs it. It does not cover ConfigMap updates, operator reconciliation, storage,
network routing, MarkLogic, or PDC business behavior.

## Run

```sh
make integration-describe SCENARIO=platform-smoke
make integration-check
make integration-test-local
INTEGRATION_CONTEXT=<explicit-context> make integration-test SCENARIO=platform-smoke
```

The live run requires kubectl and client-go access through the same context and
kubeconfig, pull access to `busybox:1.37.0`, and one schedulable Pod (requests:
10m CPU / 16Mi memory; limits: 100m / 32Mi). No operator, MarkLogic image, TLS
certificate, persistent storage, or OAuth service is needed. Expect a few minutes;
readiness is bounded to two minutes and cleanup to five minutes. The runner's
outer default timeout remains 45 minutes.

This first example uses the existing `NewRun(..., false)` Kubernetes preflight.
It checks Kubernetes discovery and SelfSubjectAccessReviews for:

- Namespace create/get/delete; Pod create/get/list/watch, exec/create and log/get.
- Events/list and PVCs/list for diagnostics and safe cleanup.
- Service, ConfigMap, Secret, and Deployment get/list/watch/create/patch.

Checks apply across generated test namespaces. These shared permissions are
broader than this example's two resources; per-scenario permission profiles are
follow-up work. Preflight grants no permissions and does not verify image pull
access or node capacity. Do not relax permissions silently when copying it.

## Copy and adapt

Copy `configmap_test.go` and `configmap_contract_test.go` to your new suite.
Change the package, unique top-level test name, gate, scenario prefix, resources,
and assertions together; replace the fixture contract test with checks relevant
to your resources. Add a matching catalog entry as described in the
[contribution guide](../../CONTRIBUTING.md). Never create cluster resources in
package initializers or `TestMain`; the live gate must be checked before setup.

`run.json` and `junit.xml` use the shared [artifact contract](../README.md#results-and-diagnostics).
A disabled direct `go test` run skips before report creation. To inspect a live
failure, use the printed run directory. `INTEGRATION_RETAIN_NAMESPACE=true` keeps
the namespace and prints context-specific inspection and cleanup commands;
otherwise cleanup verifies ownership/UID and waits for deletion. This example
creates no resources outside its owned namespace.

## Evidence to add after a live run

Record the date, source commit and uncommitted changes, exact command, Kubernetes
version/context, pulled image ID, both case outcomes, results directory, and
namespace cleanup outcome. Do not mark EKS or AKS validated from local tests.
