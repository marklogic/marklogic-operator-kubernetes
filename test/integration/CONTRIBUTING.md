# Contributing Integration Scenarios

These tests connect a product requirement to observable behavior on Kubernetes.
Start with a small scenario and reuse the existing fixtures and test utilities.
The repository [contribution policy](../../CONTRIBUTING.md) still applies. This
technical guide does not change PR acceptance policy or establish an internal
exception; maintainers must agree on any such process separately.

## Adding a scenario

1. Run `make integration-test-local` from the repository root to establish the
   local baseline; this command disables all registered live gates.
2. Start from the runnable [platform example](marklogic-server/platform/README.md).
   Copy its two Go test files into `marklogic-server/<suite>/`, change the package,
   top-level test name, unique gate, and namespace prefix, then replace the fixture
   and assertions. Copy [SCENARIO_TEMPLATE.md](SCENARIO_TEMPLATE.md) into that
   directory, complete the acceptance criteria, and link the source requirement.
3. Identify the requirements owner, test maintainer, and reviewers. The proposed
   division is for Server/PDC to own product expectations and Kubernetes
   maintainers to own deployment fixtures and cluster lifecycle. Confirm actual
   people and responsibilities in the scenario issue. Label teaching examples
   explicitly when no product requirement exists.
4. Check the live gate before any setup, report creation, or cluster access. Do
   not create cluster resources in package initializers or `TestMain` (Go test
   discovery also starts the test binary). Begin enabled runs with
   `testutil.NewRun(t, "<scenario-name>", needsMarkLogic)` and use the returned
   namespace. Apply namespaced resources through `run.ApplyObjects`, record
   phases with `run.Stage`, and assertions with `run.Case`. Never share a
   namespace or register unconditional namespace deletion. Keep one-scenario
   builders local; extract reusable builders into `fixtures/` only when needed.
5. Add one entry to [`scenarios/catalog.json`](scenarios/catalog.json), with a
   unique name, gate, and package/test pair. No runner code change is needed:

   ```json
   {
     "name": "example-check",
     "description": "Describe the observable behavior and scope.",
     "package": "./test/integration/marklogic-server/example",
     "test": "TestExampleBehavior",
     "gate": "INTEGRATION_EXAMPLE_CHECK",
     "requiredEnv": ["INTEGRATION_CONTEXT"],
     "prerequisites": ["Document permissions, images, capacity, and coverage limits."]
   }
   ```

   `package` is one repository-relative integration package; `test` is an exact
   top-level Go test name, not a regex, subtest, or package wildcard. `gate` must
   match the variable checked by the live test and must not be a configuration
   variable. Include `INTEGRATION_CONTEXT` in `requiredEnv`; MarkLogic scenarios
   also require `MARKLOGIC_IMAGE`, `INTEGRATION_OPERATOR_NAMESPACE`, and
   `INTEGRATION_OPERATOR_DEPLOYMENT`. For a minimum MarkLogic major/minor version,
   add `minMarkLogicVersion` (for example `"12.1"`) and require
   `MARKLOGIC_VERSION`. The version is a caller declaration, not image inspection.
   `prerequisites` is human-readable documentation, not an executable capability
   or permission model. Shared preflight still uses `needsMarkLogic`; document
   its actual permissions and add only capabilities your scenario needs.
6. Run `make integration-list`, `make integration-describe SCENARIO=<name>`,
   `make integration-check`, and `make integration-test-local`. Add meaningful
   local fixture/helper contracts. The catalog-driven regression automatically
   checks that each registered live suite skips before setup with gates disabled.
   Discovery rejects renamed/missing tests; the live runner also rejects skipped
   targets and zero execution. Renaming a test therefore requires a catalog edit.
7. When an appropriate cluster is available, run
   `INTEGRATION_CONTEXT=<context> make integration-test SCENARIO=<name>` with the
   declared settings. Include command, source commit/working changes, versions,
   executed cases, skips, reports, and cleanup outcome in the review. Record an
   untested environment as untested; do not infer coverage from another cloud.

## Review checklist

- The requirement has observable success and failure criteria; unrelated service
  errors cannot pass as expected authentication failures.
- The scenario has named owners and a traceable requirement, or clearly marked
  ownership gaps awaiting agreement.
- Each run owns a fresh namespace and only cleans up its own resources.
- Prerequisites, version constraints, resource needs, and coverage limits are explicit.
- Output is useful to another team and excludes credentials and session tokens.
- Shared helpers are extracted because multiple scenarios need them.

## Troubleshooting

A preflight failure identifies a missing context, permission, CRD, available
operator deployment, or storage class before deploying the workload. It cannot
prove image pull access, sufficient capacity, or operator compatibility from an
image tag alone. Diagnose workload failures from the printed results directory:
`run.json` identifies the stage and cleanup outcome, and `diagnostics.json` links
to filtered current/previous container logs. See the [artifact contract](marklogic-server/README.md#results-and-diagnostics).

Use `INTEGRATION_RETAIN_NAMESPACE=true` to retain a run for inspection. The test
prints its namespace and context-specific inspection and cleanup commands. The
normal cleanup waits up to five minutes for namespace and bound PV deletion and
fails the test if cleanup does not complete. External cloud disk deletion still
requires provider-specific verification when that is part of the acceptance criteria.
